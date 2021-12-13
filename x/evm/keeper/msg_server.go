package keeper

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tharsis/ethermint/crypto/ethsecp256k1"

	"github.com/ethereum/go-ethereum/common"

	"github.com/palantir/stacktrace"

	tmbytes "github.com/tendermint/tendermint/libs/bytes"
	tmtypes "github.com/tendermint/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/tharsis/ethermint/x/evm/types"
)

var _ types.MsgServer = &Keeper{}

// EthereumTx implements the gRPC MsgServer interface. It receives a transaction which is then
// executed (i.e applied) against the go-ethereum EVM. The provided SDK Context is set to the Keeper
// so that it can implements and call the StateDB methods without receiving it as a function
// parameter.
func (k *Keeper) EthereumTx(goCtx context.Context, msg *types.MsgEthereumTx) (*types.MsgEthereumTxResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)
	k.WithContext(ctx)

	sender := msg.From
	tx := msg.AsTransaction()

	var response *types.MsgEthereumTxResponse
	var err error
	// determine the algorithm type of the key
	/*****
	* sheldon@bianjie.ai
	**/
	ethAddr := common.HexToAddress(sender)
	cosmosAddr := sdk.AccAddress(ethAddr.Bytes())
	account := k.accountKeeper.GetAccount(ctx, cosmosAddr)
	pubKey := account.GetPubKey()
	if pubKey == nil {
		return nil, stacktrace.Propagate(types.ErrCallDisabled, "can not get publicKey for address : %s", cosmosAddr)
	}
	pubKeyAlgo := pubKey.Type()

	if pubKeyAlgo == ethsecp256k1.KeyType {
		response, err = k.ApplyTransaction(tx)
	} else {
		response, err = k.ApplyTransactionsm2(msg)
	}

	if err != nil {
		return nil, stacktrace.Propagate(err, "failed to apply transaction")
	}

	attrs := []sdk.Attribute{
		sdk.NewAttribute(sdk.AttributeKeyAmount, tx.Value().String()),
		// add event for ethereum transaction hash format
		sdk.NewAttribute(types.AttributeKeyEthereumTxHash, response.Hash),
	}

	if len(ctx.TxBytes()) > 0 {
		// add event for tendermint transaction hash format
		hash := tmbytes.HexBytes(tmtypes.Tx(ctx.TxBytes()).Hash())
		attrs = append(attrs, sdk.NewAttribute(types.AttributeKeyTxHash, hash.String()))
	}

	if tx.To() != nil {
		attrs = append(attrs, sdk.NewAttribute(types.AttributeKeyRecipient, tx.To().Hex()))
	}

	if response.Failed() {
		attrs = append(attrs, sdk.NewAttribute(types.AttributeKeyEthereumTxFailed, response.VmError))
	}

	txLogAttrs := make([]sdk.Attribute, 0)
	for _, log := range response.Logs {
		value, err := json.Marshal(log)
		if err != nil {
			return nil, stacktrace.Propagate(err, "failed to encode log")
		}
		txLogAttrs = append(txLogAttrs, sdk.NewAttribute(types.AttributeKeyTxLog, string(value)))
	}

	// emit events
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeEthereumTx,
			attrs...,
		),
		sdk.NewEvent(
			types.EventTypeTxLog,
			txLogAttrs...,
		),
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
			sdk.NewAttribute(sdk.AttributeKeySender, sender),
			sdk.NewAttribute(types.AttributeKeyTxType, fmt.Sprintf("%d", tx.Type())),
		),
	})

	return response, nil
}
