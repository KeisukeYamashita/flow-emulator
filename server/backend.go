package server

import (
	"context"
	"fmt"

	"github.com/logrusorgru/aurora"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	encoding "github.com/dapperlabs/cadence/encoding/json"
	"github.com/dapperlabs/flow-go-sdk"
	"github.com/dapperlabs/flow-go-sdk/client/protobuf/convert"
	"github.com/dapperlabs/flow-go/crypto"
	"github.com/dapperlabs/flow/protobuf/go/flow/access"
	"github.com/dapperlabs/flow/protobuf/go/flow/entities"

	emulator "github.com/dapperlabs/flow-emulator"
	"github.com/dapperlabs/flow-emulator/types"
)

// Backend wraps an emulated blockchain and implements the RPC handlers
// required by the Observation API.
type Backend struct {
	logger     *logrus.Logger
	blockchain emulator.BlockchainAPI
	automine   bool
}

// NewBackend returns a new backend.
func NewBackend(logger *logrus.Logger, blockchain emulator.BlockchainAPI) *Backend {
	return &Backend{
		logger:     logger,
		blockchain: blockchain,
		automine:   false,
	}
}

// Ping the Observation API server for a response.
func (b *Backend) Ping(ctx context.Context, req *access.PingRequest) (*access.PingResponse, error) {
	return &access.PingResponse{}, nil
}

// SendTransaction submits a transaction to the network.
func (b *Backend) SendTransaction(ctx context.Context, req *access.SendTransactionRequest) (*access.SendTransactionResponse, error) {
	txMsg := req.GetTransaction()

	tx, err := convert.MessageToTransaction(txMsg)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	err = b.blockchain.AddTransaction(tx)
	if err != nil {
		switch err.(type) {
		case *emulator.ErrDuplicateTransaction:
			return nil, status.Error(codes.InvalidArgument, err.Error())
		case *emulator.ErrInvalidSignaturePublicKey:
			return nil, status.Error(codes.InvalidArgument, err.Error())
		case *emulator.ErrInvalidSignatureAccount:
			return nil, status.Error(codes.InvalidArgument, err.Error())
		default:
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else {
		b.logger.
			WithField("txHash", tx.Hash().Hex()).
			Debug("️✉️   Transaction submitted")
	}

	response := &access.SendTransactionResponse{
		Id: tx.Hash(),
	}

	if b.automine {
		b.commitBlock()
	}

	return response, nil
}

// GetLatestBlockHeader gets the latest sealed block.
func (b *Backend) GetLatestBlockHeader(ctx context.Context, req *access.GetLatestBlockHeaderRequest) (*access.BlockHeaderResponse, error) {
	block, err := b.blockchain.GetLatestBlock()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	b.logger.WithFields(logrus.Fields{
		"blockHeight": block.Number,
		"blockHash":   block.Hash().Hex(),
	}).Debug("🎁  GetLatestBlock called")

	return b.getBlockHeaderAtBlock(block)
}

// GetBlockHeaderByHeight gets a block header by it's height
func (b *Backend) GetBlockHeaderByHeight(ctx context.Context, req *access.GetBlockHeaderByHeightRequest) (*access.BlockHeaderResponse, error) {
	block, err := b.blockchain.GetBlockByNumber(req.GetHeight())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	b.logger.WithFields(logrus.Fields{
		"blockHeight": block.Number,
		"blockHash":   block.Hash().Hex(),
	}).Debug("🎁  GetBlockHeaderByHeight called")

	return b.getBlockHeaderAtBlock(block)
}

// GetBlockHeaderByID gets a block header by it's ID
func (b *Backend) GetBlockHeaderByID(ctx context.Context, req *access.GetBlockHeaderByIDRequest) (*access.BlockHeaderResponse, error) {
	block, err := b.blockchain.GetBlockByHash(req.GetId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	b.logger.WithFields(logrus.Fields{
		"blockHeight": block.Number,
		"blockHash":   block.Hash().Hex(),
	}).Debug("🎁  GetBlockHeaderByID called")

	return b.getBlockHeaderAtBlock(block)
}

// GetLatestBlock gets the latest sealed block.
func (b *Backend) GetLatestBlock(ctx context.Context, req *access.GetLatestBlockRequest) (*access.BlockResponse, error) {
	// block, err := b.blockchain.GetLatestBlock()
	// if err != nil {
	// 	return nil, status.Error(codes.Internal, err.Error())
	// }

	// // create block header for block
	// block := flow.Block{
	// 	ID:       flow.HashToID(block.Hash()),
	// 	ParentID: flow.HashToID(block.PreviousBlockHash),
	// 	Height:   block.Number,
	// }

	// b.logger.WithFields(logrus.Fields{
	// 	"blockHeight":  blockHeader.Height,
	// 	"blockHash": blockHeader.ID,
	// }).Debug("🎁  GetLatestBlock called")

	// response := &access.BlockHeaderResponse{
	// 	Block: convert.BlockHeaderToMessage(blockHeader),
	// }
	panic("not implemented")
	return nil, nil
}

// GetBlockByHeight gets the latest sealed block.
func (b *Backend) GetBlockByHeight(ctx context.Context, req *access.GetBlockByHeightRequest) (*access.BlockResponse, error) {
	panic("not implemented")
	return nil, nil
}

// GetBlockByID gets the latest sealed block.
func (b *Backend) GetBlockByID(ctx context.Context, req *access.GetBlockByIDRequest) (*access.BlockResponse, error) {
	panic("not implemented")
	return nil, nil
}

// GetCollectionByID gets a collection by ID.
func (b *Backend) GetCollectionByID(ctx context.Context, req *access.GetCollectionByIDRequest) (*access.CollectionResponse, error) {
	panic("not implemented")
	return nil, nil
}

// GetTransaction gets a transaction by hash.
func (b *Backend) GetTransaction(ctx context.Context, req *access.GetTransactionRequest) (*access.TransactionResponse, error) {
	hash := crypto.BytesToHash(req.GetId())

	tx, err := b.blockchain.GetTransaction(hash)
	if err != nil {
		switch err.(type) {
		case *emulator.ErrTransactionNotFound:
			return nil, status.Error(codes.NotFound, err.Error())
		default:
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	b.logger.
		WithField("txHash", hash.Hex()).
		Debugf("💵  GetTransaction called")

	return &access.TransactionResponse{
		Transaction: convert.TransactionToMessage(*tx),
	}, nil
}

// GetTransactionResult gets a transaction by hash.
func (b *Backend) GetTransactionResult(ctx context.Context, req *access.GetTransactionRequest) (*access.TransactionResultResponse, error) {
	panic("not implemented")

	return nil, nil
}

// GetAccount returns the info associated with an address.
func (b *Backend) GetAccount(ctx context.Context, req *access.GetAccountRequest) (*access.GetAccountResponse, error) {
	address := flow.BytesToAddress(req.GetAddress())
	account, err := b.blockchain.GetAccount(address)
	if err != nil {
		switch err.(type) {
		case *emulator.ErrAccountNotFound:
			return nil, status.Error(codes.NotFound, err.Error())
		default:
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	b.logger.
		WithField("address", address).
		Debugf("👤  GetAccount called")

	accMsg, err := convert.AccountToMessage(*account)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &access.GetAccountResponse{
		Account: accMsg,
	}, nil
}

// ExecuteScriptAtLatestBlock executes a script at a the latest block
func (b *Backend) ExecuteScriptAtLatestBlock(ctx context.Context, req *access.ExecuteScriptAtLatestBlockRequest) (*access.ExecuteScriptResponse, error) {
	script := req.GetScript()
	block, err := b.blockchain.GetLatestBlock()
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return b.executeScriptAtBlock(script, block.Number)
}

// ExecuteScriptAtBlockHeight executes a script at a specific block height
func (b *Backend) ExecuteScriptAtBlockHeight(ctx context.Context, req *access.ExecuteScriptAtBlockHeightRequest) (*access.ExecuteScriptResponse, error) {
	script := req.GetScript()
	blockHeight := req.GetBlockHeight()
	return b.executeScriptAtBlock(script, blockHeight)
}

// ExecuteScriptAtBlockID executes a script at a specific block ID
func (b *Backend) ExecuteScriptAtBlockID(ctx context.Context, req *access.ExecuteScriptAtBlockIDRequest) (*access.ExecuteScriptResponse, error) {
	script := req.GetScript()
	blockID := req.GetBlockId()

	block, err := b.blockchain.GetBlockByHash(blockID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return b.executeScriptAtBlock(script, block.Number)
}

// GetEventsForHeightRange returns events matching a query.
func (b *Backend) GetEventsForHeightRange(ctx context.Context, req *access.GetEventsForHeightRangeRequest) (*access.EventsResponse, error) {
	// Check for invalid queries
	if req.StartHeight > req.EndHeight {
		return nil, status.Error(codes.InvalidArgument, "invalid query: start block must be <= end block")
	}

	events, err := b.blockchain.GetEvents(req.GetType(), req.GetStartHeight(), req.GetEndHeight())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	b.logger.WithFields(logrus.Fields{
		"eventType":   req.Type,
		"startHeight": req.StartHeight,
		"endHeight":   req.EndHeight,
		"results":     len(events),
	}).Debugf("🎁  GetEvents called")

	eventMessages := make([]*entities.Event, len(events))
	for i, event := range events {
		eventMessages[i], err = convert.EventToMessage(event)
		if err != nil {
			return nil, err
		}
	}

	res := access.EventsResponse{
		Events: eventMessages,
	}

	return &res, nil
}

// GetEventsForBlockIDs returns events matching a set of block IDs.
func (b *Backend) GetEventsForBlockIDs(ctx context.Context, req *access.GetEventsForBlockIDsRequest) (*access.EventsResponse, error) {
	panic("not implemented")
	return nil, nil
}

// commitBlock executes the current pending transactions and commits the results in a new block.
func (b *Backend) commitBlock() {
	block, results, err := b.blockchain.ExecuteAndCommitBlock()
	if err != nil {
		b.logger.WithError(err).Error("Failed to commit block")
		return
	}

	for _, result := range results {
		printTransactionResult(b.logger, result)
	}

	b.logger.WithFields(logrus.Fields{
		"blockHeight": block.Number,
		"blockHash":   block.Hash().Hex(),
		"blockSize":   len(block.TransactionHashes),
	}).Debugf("📦  Block #%d committed", block.Number)
}

// executeScriptAtBlock is a helper for executing a script at a specific block
func (b *Backend) executeScriptAtBlock(script []byte, blockNumber uint64) (*access.ExecuteScriptResponse, error) {
	result, err := b.blockchain.ExecuteScriptAtBlock(script, blockNumber)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	printScriptResult(b.logger, result)

	if result.Value == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid script")
	}

	valueBytes, err := encoding.Encode(result.Value)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	response := &access.ExecuteScriptResponse{
		Value: valueBytes,
	}

	return response, nil
}

// executeScriptAtBlock is a helper for getting the block header at a specific block
func (b *Backend) getBlockHeaderAtBlock(block *types.Block) (*access.BlockHeaderResponse, error) {
	// create block header for block
	blockHeader := flow.BlockHeader{
		ID:       flow.HashToID(block.Hash()),
		ParentID: flow.HashToID(block.PreviousBlockHash),
		Height:   block.Number,
	}

	response := &access.BlockHeaderResponse{
		Block: convert.BlockHeaderToMessage(blockHeader),
	}

	return response, nil
}

// EnableAutoMine enables the automine flag.
func (b *Backend) EnableAutoMine() {
	b.automine = true
}

// DisableAutoMine disables the automine flag.
func (b *Backend) DisableAutoMine() {
	b.automine = false
}

func printTransactionResult(logger *logrus.Logger, result emulator.TransactionResult) {
	if result.Succeeded() {
		logger.
			WithField("txHash", result.TransactionHash.Hex()).
			Info("⭐  Transaction executed")
	} else {
		logger.
			WithField("txHash", result.TransactionHash.Hex()).
			Warn("❗  Transaction reverted")
	}

	for _, log := range result.Logs {
		logger.Debugf(
			"%s %s",
			logPrefix("LOG", result.TransactionHash, aurora.BlueFg),
			log,
		)
	}

	for _, event := range result.Events {
		logger.Debugf(
			"%s %s",
			logPrefix("EVT", result.TransactionHash, aurora.GreenFg),
			event.String(),
		)
	}

	if result.Reverted() {
		logger.Warnf(
			"%s %s",
			logPrefix("ERR", result.TransactionHash, aurora.RedFg),
			result.Error.Error(),
		)
	}
}

func printScriptResult(logger *logrus.Logger, result emulator.ScriptResult) {
	if result.Succeeded() {
		logger.
			WithField("scriptHash", result.ScriptHash.Hex()).
			Info("⭐  Script executed")
	} else {
		logger.
			WithField("scriptHash", result.ScriptHash.Hex()).
			Warn("❗  Script reverted")
	}

	for _, log := range result.Logs {
		logger.Debugf(
			"%s %s",
			logPrefix("LOG", result.ScriptHash, aurora.BlueFg),
			log,
		)
	}

	for _, event := range result.Events {
		logger.Debugf(
			"%s %s",
			logPrefix("EVT", result.ScriptHash, aurora.GreenFg),
			event.String(),
		)
	}

	if result.Reverted() {
		logger.Warnf(
			"%s %s",
			logPrefix("ERR", result.ScriptHash, aurora.RedFg),
			result.Error.Error(),
		)
	}
}

func logPrefix(prefix string, hash crypto.Hash, color aurora.Color) string {
	prefix = aurora.Colorize(prefix, color|aurora.BoldFm).String()
	shortHash := fmt.Sprintf("[%s]", hash.Hex()[:6])
	shortHash = aurora.Colorize(shortHash, aurora.FaintFm).String()
	return fmt.Sprintf("%s %s", prefix, shortHash)
}
