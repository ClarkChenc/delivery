package checkpoint

import (
	"bytes"
	"strconv"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/maticnetwork/heimdall/checkpoint/types"
	"github.com/maticnetwork/heimdall/common"
	"github.com/maticnetwork/heimdall/helper"
	hmTypes "github.com/maticnetwork/heimdall/types"
)

// NewHandler creates new handler for handling messages for checkpoint module
func NewHandler(k Keeper, contractCaller helper.IContractCaller) sdk.Handler {
	return func(ctx sdk.Context, msg sdk.Msg) sdk.Result {
		ctx = ctx.WithEventManager(sdk.NewEventManager())
		switch msg := msg.(type) {
		case types.MsgCheckpoint:
			return handleMsgCheckpoint(ctx, msg, k, contractCaller)
		case types.MsgCheckpointAck:
			return handleMsgCheckpointAck(ctx, msg, k, contractCaller)
		case types.MsgCheckpointNoAck:
			return handleMsgCheckpointNoAck(ctx, msg, k)
		case types.MsgCheckpointSync:
			return handleMsgCheckpointSync(ctx, msg, k)
		case types.MsgCheckpointSyncAck:
			return handleMsgCheckpointSyncAck(ctx, msg, k)
		default:
			return sdk.ErrTxDecode("Invalid message in checkpoint module").Result()
		}
	}
}

// handleMsgCheckpoint Validates checkpoint transaction
func handleMsgCheckpoint(ctx sdk.Context, msg types.MsgCheckpoint, k Keeper, contractCaller helper.IContractCaller) sdk.Result {
	logger := k.Logger(ctx)

	timeStamp := uint64(ctx.BlockTime().Unix())
	params := k.GetParams(ctx)

	//
	// Check checkpoint buffer
	//
	if msg.RootChainType != hmTypes.RootChainTypeEth {
		checkpointBuffer, err := k.GetOtherCheckpointFromBuffer(ctx, msg.RootChainType)
		if err == nil {
			checkpointBufferTime := uint64(params.CheckpointBufferTime.Seconds())
			if checkpointBuffer.TimeStamp == 0 || ((timeStamp > checkpointBuffer.TimeStamp) && timeStamp-checkpointBuffer.TimeStamp >= checkpointBufferTime) {
				logger.Debug("Checkpoint has been timed out. Flushing buffer.", "checkpointTimestamp", timeStamp, "prevCheckpointTimestamp", checkpointBuffer.TimeStamp)
				k.FlushOtherCheckpointBuffer(ctx, msg.RootChainType)
			} else {
				expiryTime := checkpointBuffer.TimeStamp + checkpointBufferTime
				logger.Error("Checkpoint already exits in buffer", "root", msg.RootChainType, "Checkpoint", checkpointBuffer.String(), "Expires", expiryTime)
				return common.ErrNoACK(k.Codespace(), expiryTime).Result()
			}
		}
	} else {
		checkpointBuffer, err := k.GetCheckpointFromBuffer(ctx)
		if err == nil {
			checkpointBufferTime := uint64(params.CheckpointBufferTime.Seconds())

			if checkpointBuffer.TimeStamp == 0 || ((timeStamp > checkpointBuffer.TimeStamp) && timeStamp-checkpointBuffer.TimeStamp >= checkpointBufferTime) {
				logger.Debug("Checkpoint has been timed out. Flushing buffer.", "checkpointTimestamp", timeStamp, "prevCheckpointTimestamp", checkpointBuffer.TimeStamp)
				k.FlushCheckpointBuffer(ctx)
			} else {
				expiryTime := checkpointBuffer.TimeStamp + checkpointBufferTime
				logger.Error("Checkpoint already exits in buffer", "Checkpoint", checkpointBuffer.String(), "Expires", expiryTime)
				return common.ErrNoACK(k.Codespace(), expiryTime).Result()
			}
		}

	}

	//
	// Validate last checkpoint
	//
	var lastCheckpoint hmTypes.Checkpoint
	var err error
	if msg.RootChainType != hmTypes.RootChainTypeEth {
		lastCheckpoint, err = k.GetLastOtherCheckpoint(ctx, msg.RootChainType)
	} else {
		lastCheckpoint, err = k.GetLastCheckpoint(ctx)
	}
	// fetch last checkpoint from store
	if err == nil {
		// make sure new checkpoint is after tip
		if lastCheckpoint.EndBlock > msg.StartBlock {
			logger.Error("Checkpoint already exists",
				"currentTip", lastCheckpoint.EndBlock,
				"startBlock", msg.StartBlock,
				"root", msg.RootChainType,
			)
			return common.ErrOldCheckpoint(k.Codespace()).Result()
		}

		// check if new checkpoint's start block start from current tip
		if lastCheckpoint.EndBlock+1 != msg.StartBlock {
			logger.Error("Checkpoint not in countinuity",
				"currentTip", lastCheckpoint.EndBlock,
				"startBlock", msg.StartBlock, "root", msg.RootChainType)
			return common.ErrDisCountinuousCheckpoint(k.Codespace()).Result()
		}
	} else if err.Error() == common.ErrNoCheckpointFound(k.Codespace()).Error() && msg.StartBlock != 0 {
		logger.Error("First checkpoint to start from block 0",
			"Error", err, "root", msg.RootChainType)
		return common.ErrBadBlockDetails(k.Codespace()).Result()
	}

	//
	// Validate account hash
	//

	// Make sure latest AccountRootHash matches
	// Calculate new account root hash
	dividendAccounts := k.moduleCommunicator.GetAllDividendAccounts(ctx)
	logger.Debug("DividendAccounts of all validators", "dividendAccountsLength", len(dividendAccounts))

	// Get account root has from dividend accounts
	accountRoot, err := types.GetAccountRootHash(dividendAccounts)
	if err != nil {
		logger.Error("Error while fetching account root hash", "error", err)
		return common.ErrBadBlockDetails(k.Codespace()).Result()
	}
	logger.Debug("Validator account root hash generated", "accountRootHash", hmTypes.BytesToHeimdallHash(accountRoot).String())

	// Compare stored root hash to msg root hash
	if !bytes.Equal(accountRoot, msg.AccountRootHash.Bytes()) {
		logger.Error(
			"AccountRootHash of current state doesn't match from msg",
			"hash", hmTypes.BytesToHeimdallHash(accountRoot).String(),
			"msgHash", msg.AccountRootHash,
		)
		return common.ErrBadBlockDetails(k.Codespace()).Result()
	}

	//
	// Validate proposer
	//

	// Check proposer in message
	validatorSet := k.sk.GetValidatorSet(ctx)
	if validatorSet.Proposer == nil {
		logger.Error("No proposer in validator set", "msgProposer", msg.Proposer.String())
		return common.ErrInvalidMsg(k.Codespace(), "No proposer in stored validator set").Result()
	}

	if !bytes.Equal(msg.Proposer.Bytes(), validatorSet.Proposer.Signer.Bytes()) {
		logger.Error(
			"Invalid proposer in msg",
			"proposer", validatorSet.Proposer.Signer.String(),
			"msgProposer", msg.Proposer.String(),
		)
		return common.ErrInvalidMsg(k.Codespace(), "Invalid proposer in msg").Result()
	}

	//
	// Validate epoch
	//
	epoch := k.GetACKCount(ctx) + 1
	if epoch != msg.Epoch {
		logger.Error("Current epoch does not match msg", "msg.epoch", msg.Epoch, "current", epoch)
		return common.ErrInvalidMsg(k.Codespace(), "No proposer in stored validator set").Result()
	}

	// Emit event for checkpoint
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeCheckpoint,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
			sdk.NewAttribute(types.AttributeKeyProposer, msg.Proposer.String()),
			sdk.NewAttribute(types.AttributeKeyStartBlock, strconv.FormatUint(msg.StartBlock, 10)),
			sdk.NewAttribute(types.AttributeKeyEndBlock, strconv.FormatUint(msg.EndBlock, 10)),
			sdk.NewAttribute(types.AttributeKeyRootHash, msg.RootHash.String()),
			sdk.NewAttribute(types.AttributeKeyAccountHash, msg.AccountRootHash.String()),
		),
	})

	return sdk.Result{
		Events: ctx.EventManager().Events(),
	}
}

// handleMsgCheckpointAck Validates if checkpoint submitted on chain is valid
func handleMsgCheckpointAck(ctx sdk.Context, msg types.MsgCheckpointAck, k Keeper, contractCaller helper.IContractCaller) sdk.Result {
	logger := k.Logger(ctx)

	// Get last checkpoint from buffer

	var headerBlock *hmTypes.Checkpoint
	var err error
	if msg.RootChainType != hmTypes.RootChainTypeEth {
		headerBlock, err = k.GetOtherCheckpointFromBuffer(ctx, msg.RootChainType)
	} else {
		headerBlock, err = k.GetCheckpointFromBuffer(ctx)
	}
	if err != nil {
		logger.Error("Unable to get checkpoint", "error", err)
		return common.ErrBadAck(k.Codespace()).Result()
	}

	if msg.StartBlock != headerBlock.StartBlock {
		logger.Error("Invalid start block", "startExpected", headerBlock.StartBlock, "startReceived", msg.StartBlock)
		return common.ErrBadAck(k.Codespace()).Result()
	}

	// Return err if start and end matches but contract root hash doesn't match
	if msg.StartBlock == headerBlock.StartBlock && msg.EndBlock == headerBlock.EndBlock && !msg.RootHash.Equals(headerBlock.RootHash) {
		logger.Error("Invalid ACK",
			"startExpected", headerBlock.StartBlock,
			"startReceived", msg.StartBlock,
			"endExpected", headerBlock.EndBlock,
			"endReceived", msg.StartBlock,
			"rootExpected", headerBlock.RootHash.String(),
			"rootRecieved", msg.RootHash.String(),
			"rootChain", msg.RootChainType,
		)
		return common.ErrBadAck(k.Codespace()).Result()
	}

	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeCheckpointAck,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
			sdk.NewAttribute(types.AttributeKeyHeaderIndex, strconv.FormatUint(msg.Number, 10)),
		),
	})

	return sdk.Result{
		Events: ctx.EventManager().Events(),
	}
}

// Handles checkpoint no-ack transaction
func handleMsgCheckpointNoAck(ctx sdk.Context, msg types.MsgCheckpointNoAck, k Keeper) sdk.Result {
	logger := k.Logger(ctx)

	// Get current block time
	currentTime := ctx.BlockTime()

	// Get buffer time from params
	bufferTime := k.GetParams(ctx).CheckpointBufferTime

	// Fetch last checkpoint from store
	// TODO figure out how to handle this error
	lastCheckpoint, _ := k.GetLastCheckpoint(ctx)
	lastCheckpointTime := time.Unix(int64(lastCheckpoint.TimeStamp), 0)

	// If last checkpoint is not present or last checkpoint happens before checkpoint buffer time -- thrown an error
	if lastCheckpointTime.After(currentTime) || (currentTime.Sub(lastCheckpointTime) < bufferTime) {
		logger.Debug("Invalid No ACK -- Waiting for last checkpoint ACK")
		return common.ErrInvalidNoACK(k.Codespace()).Result()
	}

	// Check last no ack - prevents repetitive no-ack
	lastNoAck := k.GetLastNoAck(ctx)
	lastNoAckTime := time.Unix(int64(lastNoAck), 0)

	if lastNoAckTime.After(currentTime) || (currentTime.Sub(lastNoAckTime) < bufferTime) {
		logger.Debug("Too many no-ack")
		return common.ErrTooManyNoACK(k.Codespace()).Result()
	}

	// Set new last no-ack
	newLastNoAck := uint64(currentTime.Unix())
	k.SetLastNoAck(ctx, newLastNoAck)
	logger.Debug("Last No-ACK time set", "lastNoAck", newLastNoAck)

	//
	// Update to new proposer
	//

	// Increment accum (selects new proposer)
	k.sk.IncrementAccum(ctx, 1)

	// Get new proposer
	vs := k.sk.GetValidatorSet(ctx)
	newProposer := vs.GetProposer()
	logger.Debug(
		"New proposer selected",
		"validator", newProposer.Signer.String(),
		"signer", newProposer.Signer.String(),
		"power", newProposer.VotingPower,
	)

	// add events
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeCheckpointNoAck,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
			sdk.NewAttribute(types.AttributeKeyNewProposer, newProposer.Signer.String()),
		),
	})

	return sdk.Result{
		Events: ctx.EventManager().Events(),
	}
}

// handleMsgCheckpointSync Validates if checkpoint sync submitted on chain is valid
func handleMsgCheckpointSync(ctx sdk.Context, msg types.MsgCheckpointSync, k Keeper) sdk.Result {
	logger := k.Logger(ctx)
	k.Logger(ctx).Debug("✅ Validating checkpoint sync msg",
		"root", msg.RootChainType,
		"number", msg.Number,
	)
	timeStamp := uint64(ctx.BlockTime().Unix())
	params := k.GetParams(ctx)
	//
	// Check checkpoint sync buffer
	//
	bufferSync, err := k.GetCheckpointSyncFromBuffer(ctx, msg.RootChainType)
	if err == nil {
		checkpointBufferTime := uint64(params.CheckpointBufferTime.Seconds())
		if bufferSync.TimeStamp == 0 || ((timeStamp > bufferSync.TimeStamp) && timeStamp-bufferSync.TimeStamp >= checkpointBufferTime) {
			logger.Debug("Checkpoint sync has been timed out. Flushing buffer.", "root", msg.RootChainType)
			k.FlushCheckpointSyncBuffer(ctx, msg.RootChainType)
		} else {
			expiryTime := bufferSync.TimeStamp + checkpointBufferTime
			logger.Error("Checkpoint sync already exits in buffer", "root", msg.RootChainType, "now", timeStamp, "Expires", expiryTime)
			return common.ErrNoACK(k.Codespace(), expiryTime).Result()
		}
	}

	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeCheckpointSync,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
			sdk.NewAttribute(types.AttributeKeyProposer, msg.Proposer.String()),
			sdk.NewAttribute(types.AttributeKeyStartBlock, strconv.FormatUint(msg.StartBlock, 10)),
			sdk.NewAttribute(types.AttributeKeyEndBlock, strconv.FormatUint(msg.EndBlock, 10)),
		),
	})

	return sdk.Result{
		Events: ctx.EventManager().Events(),
	}
}

// handleMsgCheckpointSyncAck Validates if checkpoint sync submitted on chain is valid
func handleMsgCheckpointSyncAck(ctx sdk.Context, msg types.MsgCheckpointSyncAck, k Keeper) sdk.Result {
	logger := k.Logger(ctx)
	k.Logger(ctx).Debug("✅ Validating checkpoint sync ack msg",
		"root", msg.RootChainType,
		"number", msg.Number,
	)
	timeStamp := uint64(ctx.BlockTime().Unix())
	params := k.GetParams(ctx)
	//
	// Check checkpoint sync buffer
	//
	bufferSync, err := k.GetCheckpointSyncFromBuffer(ctx, msg.RootChainType)
	if err == nil {
		checkpointBufferTime := uint64(params.CheckpointBufferTime.Seconds())
		if bufferSync.TimeStamp == 0 || ((timeStamp > bufferSync.TimeStamp) && timeStamp-bufferSync.TimeStamp >= checkpointBufferTime) {
			logger.Debug("Checkpoint sync has been timed out. Flushing buffer.", "checkpointTimestamp", timeStamp, "prevCheckpointTimestamp", bufferSync.TimeStamp)
			k.FlushCheckpointSyncBuffer(ctx, msg.RootChainType)
		}
	}

	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeCheckpointSyncAck,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
			sdk.NewAttribute(types.AttributeKeyProposer, msg.Proposer.String()),
			sdk.NewAttribute(types.AttributeKeyStartBlock, strconv.FormatUint(msg.StartBlock, 10)),
			sdk.NewAttribute(types.AttributeKeyEndBlock, strconv.FormatUint(msg.EndBlock, 10)),
		),
	})

	return sdk.Result{
		Events: ctx.EventManager().Events(),
	}
}
