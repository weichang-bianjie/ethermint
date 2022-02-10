package keeper

import (
	"math/big"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/tharsis/ethermint/x/evm/types"
)

// ApplyTransactionSm2 runs and attempts to perform a state transition with the given transaction (i.e Message), that will
// only be persisted (committed) to the underlying KVStore if the transaction does not fail.
//
// Gas tracking
//
// Ethereum consumes gas according to the EVM opcodes instead of general reads and writes to store. Because of this, the
// state transition needs to ignore the SDK gas consumption mechanism defined by the GasKVStore and instead consume the
// amount of gas used by the VM execution. The amount of gas used is tracked by the EVM and returned in the execution
// result.
//
// Prior to the execution, the starting tx gas meter is saved and replaced with an infinite gas meter in a new context
// in order to ignore the SDK gas consumption config values (read, write, has, delete).
// After the execution, the gas used from the message execution will be added to the starting gas consumed, taking into
// consideration the amount of gas returned. Finally, the context is updated with the EVM gas consumed value prior to
// returning.
//
// For relevant discussion see: https://github.com/cosmos/cosmos-sdk/discussions/9072
func (k *Keeper) ApplyTransactionSm2(tx *types.MsgEthereumTx) (*types.MsgEthereumTxResponse, error) {
	ctx := k.Ctx()

	// ensure keeper state error is cleared
	defer k.ClearStateError()

	cfg, err := k.EVMConfig(ctx)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "failed to load evm config")
	}

	// get the latest signer according to the chain rules from the config
	//signer := ethtypes.MakeSigner(cfg.ChainConfig, big.NewInt(ctx.BlockHeight()))

	signer := k.Signer

	var baseFee *big.Int
	if types.IsLondon(cfg.ChainConfig, ctx.BlockHeight()) {
		baseFee = k.feeMarketKeeper.GetBaseFee(ctx)
	}

	msg, err := tx.AsMessage(signer, baseFee)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "failed to return ethereum transaction as core message")
	}

	txHash := common.HexToHash(tx.Hash)

	// set the transaction hash and index to the impermanent (transient) block state so that it's also
	// available on the StateDB functions (eg: AddLog)
	k.SetTxHashTransient(txHash)

	// snapshot to contain the tx processing and post processing in same scope
	var commit func()
	if k.hooks != nil {
		// Create a cache context to revert state when tx hooks fails,
		// the cache context is only committed when both tx and hooks executed successfully.
		// Didn't use `Snapshot` because the context stack has exponential complexity on certain operations,
		// thus restricted to be used only inside `ApplyMessage`.
		var cacheCtx sdk.Context
		cacheCtx, commit = ctx.CacheContext()
		k.WithContext(cacheCtx)
		defer (func() {
			k.WithContext(ctx)
		})()
	}

	res, err := k.ApplyMessageWithConfig(msg, nil, true, cfg)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "failed to apply ethereum core message")
	}

	res.Hash = txHash.Hex()

	logs := k.GetTxLogsTransient(txHash)

	if !res.Failed() {
		// Only call hooks if tx executed successfully.
		if err = k.PostTxProcessing(txHash, logs); err != nil {
			// If hooks return error, revert the whole tx.
			res.VmError = types.ErrPostTxProcessing.Error()
			k.Logger(k.Ctx()).Error("tx post processing failed", "error", err)
		} else if commit != nil {
			// PostTxProcessing is successful, commit the cache context
			commit()
			ctx.EventManager().EmitEvents(k.Ctx().EventManager().Events())
		}
	}

	// change to original context
	k.WithContext(ctx)

	// refund gas according to Ethereum gas accounting rules.
	if err := k.RefundGas(msg, msg.Gas()-res.GasUsed, cfg.Params.EvmDenom); err != nil {
		return nil, sdkerrors.Wrapf(err, "failed to refund gas leftover gas to sender %s", msg.From())
	}

	if len(logs) > 0 {
		res.Logs = types.NewLogsFromEth(logs)
		// Update transient block bloom filter
		bloom := k.GetBlockBloomTransient()
		bloom.Or(bloom, big.NewInt(0).SetBytes(ethtypes.LogsBloom(logs)))
		k.SetBlockBloomTransient(bloom)
	}

	k.IncreaseTxIndexTransient()

	// update the gas used after refund
	k.ResetGasMeterAndConsumeGas(res.GasUsed)
	return res, nil
}
