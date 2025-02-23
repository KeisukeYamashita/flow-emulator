// Package emulator provides an emulated version of the Flow blockchain that can be used
// for development purposes.
//
// This package can be used as a library or as a standalone application.
//
// When used as a library, this package provides tools to write programmatic tests for
// Flow applications.
//
// When used as a standalone application, this package implements the Flow Access API
// and is fully-compatible with Flow gRPC client libraries.
package emulator

import (
	"errors"
	"fmt"
	"sync"
	"time"

	fvmcrypto "github.com/onflow/flow-go/fvm/crypto"

	"github.com/onflow/cadence"
	"github.com/onflow/cadence/runtime"
	sdk "github.com/onflow/flow-go-sdk"
	sdkcrypto "github.com/onflow/flow-go-sdk/crypto"
	"github.com/onflow/flow-go-sdk/templates"
	"github.com/onflow/flow-go/access"
	"github.com/onflow/flow-go/crypto"
	"github.com/onflow/flow-go/crypto/hash"
	"github.com/onflow/flow-go/engine/execution/state/delta"
	"github.com/onflow/flow-go/fvm"
	fvmerrors "github.com/onflow/flow-go/fvm/errors"
	"github.com/onflow/flow-go/fvm/programs"
	"github.com/onflow/flow-go/fvm/state"
	flowgo "github.com/onflow/flow-go/model/flow"
	"github.com/rs/zerolog"

	"github.com/onflow/flow-emulator/convert"
	sdkconvert "github.com/onflow/flow-emulator/convert/sdk"
	"github.com/onflow/flow-emulator/storage"
	"github.com/onflow/flow-emulator/storage/memstore"
	"github.com/onflow/flow-emulator/types"
)

// Blockchain emulates the functionality of the Flow blockchain.
type Blockchain struct {
	// committed chain state: blocks, transactions, registers, events
	storage storage.Store

	// mutex protecting pending block
	mu sync.RWMutex

	// pending block containing block info, register state, pending transactions
	pendingBlock *pendingBlock

	// used to execute transactions and scripts
	vm    *fvm.VirtualMachine
	vmCtx fvm.Context

	transactionValidator *access.TransactionValidator

	serviceKey ServiceKey
}

type ServiceKey struct {
	Index          int
	Address        sdk.Address
	SequenceNumber uint64
	PrivateKey     sdkcrypto.PrivateKey
	PublicKey      sdkcrypto.PublicKey
	HashAlgo       sdkcrypto.HashAlgorithm
	SigAlgo        sdkcrypto.SignatureAlgorithm
	Weight         int
}

func (s ServiceKey) Signer() sdkcrypto.Signer {
	return sdkcrypto.NewInMemorySigner(s.PrivateKey, s.HashAlgo)
}

func (s ServiceKey) AccountKey() *sdk.AccountKey {

	var publicKey sdkcrypto.PublicKey
	if s.PublicKey != nil {
		publicKey = s.PublicKey
	}

	if s.PrivateKey != nil {
		publicKey = s.PrivateKey.PublicKey()
	}

	return &sdk.AccountKey{
		Index:          s.Index,
		PublicKey:      publicKey,
		SigAlgo:        s.SigAlgo,
		HashAlgo:       s.HashAlgo,
		Weight:         s.Weight,
		SequenceNumber: s.SequenceNumber,
	}
}

const defaultServiceKeyPrivateKeySeed = "elephant ears space cowboy octopus rodeo potato cannon pineapple"
const DefaultServiceKeySigAlgo = sdkcrypto.ECDSA_P256
const DefaultServiceKeyHashAlgo = sdkcrypto.SHA3_256

func DefaultServiceKey() ServiceKey {
	return GenerateDefaultServiceKey(DefaultServiceKeySigAlgo, DefaultServiceKeyHashAlgo)
}

func GenerateDefaultServiceKey(
	sigAlgo sdkcrypto.SignatureAlgorithm,
	hashAlgo sdkcrypto.HashAlgorithm,
) ServiceKey {
	privateKey, err := sdkcrypto.GeneratePrivateKey(
		sigAlgo,
		[]byte(defaultServiceKeyPrivateKeySeed),
	)
	if err != nil {
		panic(fmt.Sprintf("Failed to generate default service key: %s", err.Error()))
	}

	return ServiceKey{
		PrivateKey: privateKey,
		SigAlgo:    sigAlgo,
		HashAlgo:   hashAlgo,
	}
}

// config is a set of configuration options for an emulated blockchain.
type config struct {
	ServiceKey                ServiceKey
	Store                     storage.Store
	SimpleAddresses           bool
	GenesisTokenSupply        cadence.UFix64
	TransactionMaxGasLimit    uint64
	ScriptGasLimit            uint64
	TransactionExpiry         uint
	StorageLimitEnabled       bool
	TransactionFeesEnabled    bool
	MinimumStorageReservation cadence.UFix64
	StorageMBPerFLOW          cadence.UFix64
}

func (conf config) GetStore() storage.Store {
	// if no store is specified, use a memstore
	// NOTE: we don't initialize this in defaultConfig because otherwise the same
	// memstore is shared between Blockchain instances
	if conf.Store == nil {
		return memstore.New()
	}

	return conf.Store
}

func (conf config) GetChainID() flowgo.ChainID {
	if conf.SimpleAddresses {
		return flowgo.MonotonicEmulator
	}

	return flowgo.Emulator
}

func (conf config) GetServiceKey() ServiceKey {
	// set up service key
	serviceKey := conf.ServiceKey
	serviceKey.Address = sdk.Address(conf.GetChainID().Chain().ServiceAddress())
	serviceKey.Weight = sdk.AccountKeyWeightThreshold

	return serviceKey
}

const defaultGenesisTokenSupply = "1000000000.0"
const defaultScriptGasLimit = 100000
const defaultTransactionMaxGasLimit = flowgo.DefaultMaxTransactionGasLimit

// defaultConfig is the default configuration for an emulated blockchain.
var defaultConfig = func() config {
	genesisTokenSupply, err := cadence.NewUFix64(defaultGenesisTokenSupply)
	if err != nil {
		panic(fmt.Sprintf("Failed to parse default genesis token supply: %s", err.Error()))
	}

	return config{
		ServiceKey:                DefaultServiceKey(),
		Store:                     nil,
		SimpleAddresses:           false,
		GenesisTokenSupply:        genesisTokenSupply,
		ScriptGasLimit:            defaultScriptGasLimit,
		TransactionMaxGasLimit:    defaultTransactionMaxGasLimit,
		MinimumStorageReservation: fvm.DefaultMinimumStorageReservation,
		StorageMBPerFLOW:          fvm.DefaultStorageMBPerFLOW,
		TransactionExpiry:         0, // TODO: replace with sensible default
		StorageLimitEnabled:       true,
	}
}()

// Option is a function applying a change to the emulator config.
type Option func(*config)

// WithServicePublicKey sets the service key from a public key.
func WithServicePublicKey(
	servicePublicKey sdkcrypto.PublicKey,
	sigAlgo sdkcrypto.SignatureAlgorithm,
	hashAlgo sdkcrypto.HashAlgorithm,
) Option {
	return func(c *config) {
		c.ServiceKey = ServiceKey{
			PublicKey: servicePublicKey,
			SigAlgo:   sigAlgo,
			HashAlgo:  hashAlgo,
		}
	}
}

// WithServicePrivateKey sets the service key from private key.
func WithServicePrivateKey(
	privateKey sdkcrypto.PrivateKey,
	sigAlgo sdkcrypto.SignatureAlgorithm,
	hashAlgo sdkcrypto.HashAlgorithm,
) Option {
	return func(c *config) {
		c.ServiceKey = ServiceKey{
			PrivateKey: privateKey,
			PublicKey:  privateKey.PublicKey(),
			HashAlgo:   hashAlgo,
			SigAlgo:    sigAlgo,
		}
	}
}

// WithStore sets the persistent storage provider.
func WithStore(store storage.Store) Option {
	return func(c *config) {
		c.Store = store
	}
}

// WithSimpleAddresses enables simple addresses, which are sequential starting with 0x01.
func WithSimpleAddresses() Option {
	return func(c *config) {
		c.SimpleAddresses = true
	}
}

// WithGenesisTokenSupply sets the genesis token supply.
func WithGenesisTokenSupply(supply cadence.UFix64) Option {
	return func(c *config) {
		c.GenesisTokenSupply = supply
	}
}

// WithTransactionMaxGasLimit sets the maximum gas limit for transactions.
//
// Individual transactions will still be bounded by the limit they declare.
// This function sets the maximum limit that any transaction can declare.
//
// This limit does not affect script executions. Use WithScriptGasLimit
// to set the gas limit for script executions.
func WithTransactionMaxGasLimit(maxLimit uint64) Option {
	return func(c *config) {
		c.TransactionMaxGasLimit = maxLimit
	}
}

// WithScriptGasLimit sets the gas limit for scripts.
//
// This limit does not affect transactions, which declare their own limit.
// Use WithTransactionMaxGasLimit to set the maximum gas limit for transactions.
func WithScriptGasLimit(limit uint64) Option {
	return func(c *config) {
		c.ScriptGasLimit = limit
	}
}

// WithTransactionExpiry sets the transaction expiry measured in blocks.
//
// If set to zero, transaction expiry is disabled and the reference block ID field
// is not required.
func WithTransactionExpiry(expiry uint) Option {
	return func(c *config) {
		c.TransactionExpiry = expiry
	}
}

// WithStorageLimitEnabled enables/disables limiting account storage used to their storage capacity.
//
// If set to false, accounts can store any amount of data,
// otherwise they can only store as much as their storage capacity.
// The default is true.
func WithStorageLimitEnabled(enabled bool) Option {
	return func(c *config) {
		c.StorageLimitEnabled = enabled
	}
}

// WithMinimumStorageReservation sets the minimum account balance.
//
// The cost of creating new accounts is also set to this value.
// The default is taken from fvm.DefaultMinimumStorageReservation
func WithMinimumStorageReservation(minimumStorageReservation cadence.UFix64) Option {
	return func(c *config) {
		c.MinimumStorageReservation = minimumStorageReservation
	}
}

// WithStorageMBPerFLOW sets the cost of a megabyte of storage in FLOW
//
// the default is taken from fvm.DefaultStorageMBPerFLOW
func WithStorageMBPerFLOW(storageMBPerFLOW cadence.UFix64) Option {
	return func(c *config) {
		c.StorageMBPerFLOW = storageMBPerFLOW
	}
}

// WithTransactionFeesEnabled enables/disables transaction fees.
//
// If set to false transactions don't cost any flow.
// The default is false.
func WithTransactionFeesEnabled(enabled bool) Option {
	return func(c *config) {
		c.TransactionFeesEnabled = enabled
	}
}

// NewBlockchain instantiates a new emulated blockchain with the provided options.
func NewBlockchain(opts ...Option) (*Blockchain, error) {

	// apply options to the default config
	conf := defaultConfig
	for _, opt := range opts {
		opt(&conf)
	}

	b := &Blockchain{
		storage:    conf.GetStore(),
		serviceKey: conf.GetServiceKey(),
	}

	var err error

	blocks := newBlocks(b)

	b.vm, b.vmCtx, err = configureFVM(conf, blocks)
	if err != nil {
		return nil, err
	}

	latestBlock, latestLedgerView, err := configureLedger(conf, b.storage, b.vm, b.vmCtx)
	if err != nil {
		return nil, err
	}

	b.pendingBlock = newPendingBlock(latestBlock, latestLedgerView)
	b.transactionValidator = configureTransactionValidator(conf, blocks)

	return b, nil
}

func configureFVM(conf config, blocks *blocks) (*fvm.VirtualMachine, fvm.Context, error) {
	rt := runtime.NewInterpreterRuntime()

	vm := fvm.NewVirtualMachine(rt)

	ctx := fvm.NewContext(
		zerolog.Nop(),
		fvm.WithChain(conf.GetChainID().Chain()),
		fvm.WithBlocks(blocks),
		fvm.WithRestrictedDeployment(false),
		fvm.WithGasLimit(conf.ScriptGasLimit),
		fvm.WithCadenceLogging(true),
		fvm.WithAccountStorageLimit(conf.StorageLimitEnabled),
		fvm.WithTransactionFeesEnabled(conf.TransactionFeesEnabled),
	)

	return vm, ctx, nil
}

func configureLedger(
	conf config,
	store storage.Store,
	vm *fvm.VirtualMachine,
	ctx fvm.Context,
) (*flowgo.Block, *delta.View, error) {
	latestBlock, err := store.LatestBlock()
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// storage is empty, bootstrap new ledger state
			return configureNewLedger(conf, store, vm, ctx)
		}

		// internal storage error, fail fast
		return nil, nil, err
	}

	// storage contains data, load state from storage
	return configureExistingLedger(&latestBlock, store)
}

func configureNewLedger(
	conf config,
	store storage.Store,
	vm *fvm.VirtualMachine,
	ctx fvm.Context,
) (*flowgo.Block, *delta.View, error) {
	genesisLedgerView := store.LedgerViewByHeight(0)

	err := bootstrapLedger(
		vm,
		ctx,
		genesisLedgerView,
		conf,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to bootstrap execution state: %w", err)
	}

	// commit the genesis block to storage
	genesis := flowgo.Genesis(conf.GetChainID())

	err = store.CommitBlock(
		*genesis,
		nil,
		nil,
		nil,
		genesisLedgerView.Delta(),
		nil,
	)
	if err != nil {
		return nil, nil, err
	}

	// get empty ledger view
	ledgerView := store.LedgerViewByHeight(0)

	return genesis, ledgerView, nil
}

func configureExistingLedger(
	latestBlock *flowgo.Block,
	store storage.Store,
) (*flowgo.Block, *delta.View, error) {
	latestLedgerView := store.LedgerViewByHeight(latestBlock.Header.Height)

	return latestBlock, latestLedgerView, nil
}

func bootstrapLedger(
	vm *fvm.VirtualMachine,
	ctx fvm.Context,
	ledger state.View,
	conf config,
) error {
	accountKey := conf.GetServiceKey().AccountKey()
	publicKey, _ := crypto.DecodePublicKey(
		accountKey.SigAlgo,
		accountKey.PublicKey.Encode(),
	)

	ctx = fvm.NewContextFromParent(
		ctx,
		fvm.WithAccountStorageLimit(false),
	)

	flowAccountKey := flowgo.AccountPublicKey{
		PublicKey: publicKey,
		SignAlgo:  accountKey.SigAlgo,
		HashAlgo:  accountKey.HashAlgo,
		Weight:    fvm.AccountKeyWeightThreshold,
	}

	bootstrap := configureBootstrapProcedure(conf, flowAccountKey, conf.GenesisTokenSupply)

	err := vm.Run(ctx, bootstrap, ledger, programs.NewEmptyPrograms())
	if err != nil {
		return err
	}

	return nil
}

func configureBootstrapProcedure(conf config, flowAccountKey flowgo.AccountPublicKey, supply cadence.UFix64) *fvm.BootstrapProcedure {
	options := make([]fvm.BootstrapProcedureOption, 0)
	options = append(options,
		fvm.WithInitialTokenSupply(supply),
		fvm.WithRestrictedAccountCreationEnabled(false),
	)
	if conf.StorageLimitEnabled {
		options = append(options,
			fvm.WithAccountCreationFee(conf.MinimumStorageReservation),
			fvm.WithMinimumStorageReservation(conf.MinimumStorageReservation),
			fvm.WithStorageMBPerFLOW(conf.StorageMBPerFLOW),
		)
	}
	if conf.TransactionFeesEnabled {
		options = append(options,
			fvm.WithTransactionFee(fvm.DefaultTransactionFees),
		)
	}
	return fvm.Bootstrap(
		flowAccountKey,
		options...,
	)
}

func configureTransactionValidator(conf config, blocks *blocks) *access.TransactionValidator {
	return access.NewTransactionValidator(
		blocks,
		conf.GetChainID().Chain(),
		access.TransactionValidationOptions{
			Expiry:                       conf.TransactionExpiry,
			ExpiryBuffer:                 0,
			AllowEmptyReferenceBlockID:   conf.TransactionExpiry == 0,
			AllowUnknownReferenceBlockID: false,
			MaxGasLimit:                  conf.TransactionMaxGasLimit,
			CheckScriptsParse:            true,
			MaxTransactionByteSize:       flowgo.DefaultMaxTransactionByteSize,
			MaxCollectionByteSize:        flowgo.DefaultMaxCollectionByteSize,
		},
	)
}

// ServiceKey returns the service private key for this blockchain.
func (b *Blockchain) ServiceKey() ServiceKey {
	serviceAccount, err := b.getAccount(sdkconvert.SDKAddressToFlow(b.serviceKey.Address))
	if err != nil {
		return b.serviceKey
	}

	if len(serviceAccount.Keys) > 0 {
		b.serviceKey.Index = 0
		b.serviceKey.SequenceNumber = serviceAccount.Keys[0].SeqNumber
		b.serviceKey.Weight = serviceAccount.Keys[0].Weight
	}

	return b.serviceKey
}

// PendingBlockID returns the ID of the pending block.
func (b *Blockchain) PendingBlockID() flowgo.Identifier {
	return b.pendingBlock.ID()
}

// PendingBlockView returns the view of the pending block.
func (b *Blockchain) PendingBlockView() uint64 {
	return b.pendingBlock.view
}

// PendingBlockTimestamp returns the Timestamp of the pending block.
func (b *Blockchain) PendingBlockTimestamp() time.Time {
	return b.pendingBlock.Block().Header.Timestamp
}

// GetLatestBlock gets the latest sealed block.
func (b *Blockchain) GetLatestBlock() (*flowgo.Block, error) {
	block, err := b.storage.LatestBlock()
	if err != nil {
		return nil, &StorageError{err}
	}

	return &block, nil
}

// GetBlockByID gets a block by ID.
func (b *Blockchain) GetBlockByID(id sdk.Identifier) (*flowgo.Block, error) {
	block, err := b.storage.BlockByID(sdkconvert.SDKIdentifierToFlow(id))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &BlockNotFoundByIDError{ID: id}
		}

		return nil, &StorageError{err}
	}

	return block, nil
}

// GetBlockByHeight gets a block by height.
func (b *Blockchain) GetBlockByHeight(height uint64) (*flowgo.Block, error) {
	block, err := b.getBlockByHeight(height)
	if err != nil {
		return nil, err
	}

	return block, nil
}

func (b *Blockchain) getBlockByHeight(height uint64) (*flowgo.Block, error) {
	block, err := b.storage.BlockByHeight(height)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &BlockNotFoundByHeightError{Height: height}
		}
		return nil, err
	}

	return block, nil
}

func (b *Blockchain) GetChain() flowgo.Chain {
	return b.vmCtx.Chain
}

func (b *Blockchain) GetCollection(colID sdk.Identifier) (*sdk.Collection, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	col, err := b.storage.CollectionByID(sdkconvert.SDKIdentifierToFlow(colID))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &CollectionNotFoundError{ID: colID}
		}
		return nil, &StorageError{err}
	}

	sdkCol := sdkconvert.FlowLightCollectionToSDK(col)

	return &sdkCol, nil
}

// GetTransaction gets an existing transaction by ID.
//
// The function first looks in the pending block, then the current blockchain state.
func (b *Blockchain) GetTransaction(id sdk.Identifier) (*sdk.Transaction, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	txID := sdkconvert.SDKIdentifierToFlow(id)

	pendingTx := b.pendingBlock.GetTransaction(txID)
	if pendingTx != nil {
		pendingSDKTx := sdkconvert.FlowTransactionToSDK(*pendingTx)
		return &pendingSDKTx, nil
	}

	tx, err := b.storage.TransactionByID(txID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &TransactionNotFoundError{ID: txID}
		}
		return nil, &StorageError{err}
	}

	sdkTx := sdkconvert.FlowTransactionToSDK(tx)
	return &sdkTx, nil
}

func (b *Blockchain) GetTransactionResult(ID sdk.Identifier) (*sdk.TransactionResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	txID := sdkconvert.SDKIdentifierToFlow(ID)

	if b.pendingBlock.ContainsTransaction(txID) {
		return &sdk.TransactionResult{
			Status: sdk.TransactionStatusPending,
		}, nil
	}

	storedResult, err := b.storage.TransactionResultByID(txID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return &sdk.TransactionResult{
				Status: sdk.TransactionStatusUnknown,
			}, nil
		}
		return nil, &StorageError{err}
	}

	var errResult error

	if storedResult.ErrorCode != 0 {
		errResult = &ExecutionError{
			Code:    storedResult.ErrorCode,
			Message: storedResult.ErrorMessage,
		}
	}

	sdkEvents, err := sdkconvert.FlowEventsToSDK(storedResult.Events)
	if err != nil {
		return nil, err
	}

	result := sdk.TransactionResult{
		Status: sdk.TransactionStatusSealed,
		Error:  errResult,
		Events: sdkEvents,
	}

	return &result, nil
}

// GetAccount returns the account for the given address.
func (b *Blockchain) GetAccount(address sdk.Address) (*sdk.Account, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	flowAddress := sdkconvert.SDKAddressToFlow(address)

	account, err := b.getAccount(flowAddress)
	if err != nil {
		return nil, err
	}

	sdkAccount, err := sdkconvert.FlowAccountToSDK(*account)
	if err != nil {
		return nil, err
	}

	return &sdkAccount, nil
}

// getAccount returns the account for the given address.
func (b *Blockchain) getAccount(address flowgo.Address) (*flowgo.Account, error) {
	latestBlock, err := b.GetLatestBlock()
	if err != nil {
		return nil, err
	}

	return b.getAccountAtBlock(address, latestBlock.Header.Height)
}

// GetAccountAtBlock returns the account for the given address at specified block height.
func (b *Blockchain) GetAccountAtBlock(address sdk.Address, blockHeight uint64) (*sdk.Account, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	flowAddress := sdkconvert.SDKAddressToFlow(address)

	account, err := b.getAccountAtBlock(flowAddress, blockHeight)
	if err != nil {
		return nil, err
	}

	sdkAccount, err := sdkconvert.FlowAccountToSDK(*account)
	if err != nil {
		return nil, err
	}

	return &sdkAccount, nil
}

// GetAccountAtBlock returns the account for the given address at specified block height.
func (b *Blockchain) getAccountAtBlock(address flowgo.Address, blockHeight uint64) (*flowgo.Account, error) {

	account, err := b.vm.GetAccount(
		b.vmCtx,
		address,
		b.storage.LedgerViewByHeight(blockHeight),
		programs.NewEmptyPrograms(),
	)

	if fvmerrors.IsAccountNotFoundError(err) {
		return nil, &AccountNotFoundError{Address: address}
	}

	return account, nil
}

// GetEventsByHeight returns the events in the block at the given height, optionally filtered by type.
func (b *Blockchain) GetEventsByHeight(blockHeight uint64, eventType string) ([]sdk.Event, error) {
	flowEvents, err := b.storage.EventsByHeight(blockHeight, eventType)
	if err != nil {
		return nil, err
	}

	sdkEvents, err := sdkconvert.FlowEventsToSDK(flowEvents)
	if err != nil {
		return nil, fmt.Errorf("could not convert events: %w", err)
	}

	return sdkEvents, err
}

// AddTransaction validates a transaction and adds it to the current pending block.
func (b *Blockchain) AddTransaction(tx sdk.Transaction) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.addTransaction(tx)
}

// AddTransaction validates a transaction and adds it to the current pending block.
func (b *Blockchain) addTransaction(sdkTx sdk.Transaction) error {

	tx := sdkconvert.SDKTransactionToFlow(sdkTx)

	// If index > 0, pending block has begun execution (cannot add more transactions)
	if b.pendingBlock.ExecutionStarted() {
		return &PendingBlockMidExecutionError{BlockID: b.pendingBlock.ID()}
	}

	if b.pendingBlock.ContainsTransaction(tx.ID()) {
		return &DuplicateTransactionError{TxID: tx.ID()}
	}

	_, err := b.storage.TransactionByID(tx.ID())
	if err == nil {
		// Found the transaction, this is a duplicate
		return &DuplicateTransactionError{TxID: tx.ID()}
	} else if !errors.Is(err, storage.ErrNotFound) {
		// Error in the storage provider
		return fmt.Errorf("failed to check storage for transaction %w", err)
	}

	err = b.transactionValidator.Validate(tx)
	if err != nil {
		return convertAccessError(err)
	}

	// add transaction to pending block
	b.pendingBlock.AddTransaction(*tx)

	return nil
}

// ExecuteBlock executes the remaining transactions in pending block.
func (b *Blockchain) ExecuteBlock() ([]*types.TransactionResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.executeBlock()
}

func (b *Blockchain) executeBlock() ([]*types.TransactionResult, error) {
	results := make([]*types.TransactionResult, 0)

	// empty blocks do not require execution, treat as a no-op
	if b.pendingBlock.Empty() {
		return results, nil
	}

	header := b.pendingBlock.Block().Header
	blockContext := fvm.NewContextFromParent(
		b.vmCtx,
		fvm.WithBlockHeader(header),
	)

	// cannot execute a block that has already executed
	if b.pendingBlock.ExecutionComplete() {
		return results, &PendingBlockTransactionsExhaustedError{
			BlockID: b.pendingBlock.ID(),
		}
	}

	// continue executing transactions until execution is complete
	for !b.pendingBlock.ExecutionComplete() {
		result, err := b.executeNextTransaction(blockContext)
		if err != nil {
			return results, err
		}

		results = append(results, result)
	}

	return results, nil
}

// ExecuteNextTransaction executes the next indexed transaction in pending block.
func (b *Blockchain) ExecuteNextTransaction() (*types.TransactionResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	header := b.pendingBlock.Block().Header
	blockContext := fvm.NewContextFromParent(
		b.vmCtx,
		fvm.WithBlockHeader(header),
	)

	return b.executeNextTransaction(blockContext)
}

// executeNextTransaction is a helper function for ExecuteBlock and ExecuteNextTransaction that
// executes the next transaction in the pending block.
func (b *Blockchain) executeNextTransaction(ctx fvm.Context) (*types.TransactionResult, error) {
	// check if there are remaining txs to be executed
	if b.pendingBlock.ExecutionComplete() {
		return nil, &PendingBlockTransactionsExhaustedError{
			BlockID: b.pendingBlock.ID(),
		}
	}

	// use the computer to execute the next transaction
	tp, err := b.pendingBlock.ExecuteNextTransaction(
		func(
			ledgerView state.View,
			txIndex uint32,
			txBody *flowgo.TransactionBody,
		) (*fvm.TransactionProcedure, error) {
			tx := fvm.Transaction(txBody, txIndex)

			err := b.vm.Run(ctx, tx, ledgerView, programs.NewEmptyPrograms())
			if err != nil {
				return nil, err
			}
			return tx, nil
		},
	)
	if err != nil {
		// fail fast if fatal error occurs
		return nil, err
	}

	tr, err := convert.VMTransactionResultToEmulator(tp)
	if err != nil {
		// fail fast if fatal error occurs
		return nil, err
	}

	// if transaction error exist try to further debug what was the problem
	if tr.Error != nil {
		tr.Debug = b.debugSignatureError(tr.Error, tp.Transaction)
	}

	return tr, nil
}

// CommitBlock seals the current pending block and saves it to storage.
//
// This function clears the pending transaction pool and resets the pending block.
func (b *Blockchain) CommitBlock() (*flowgo.Block, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	block, err := b.commitBlock()
	if err != nil {
		return nil, err
	}

	return block, nil
}

func (b *Blockchain) commitBlock() (*flowgo.Block, error) {
	// pending block cannot be committed before execution starts (unless empty)
	if !b.pendingBlock.ExecutionStarted() && !b.pendingBlock.Empty() {
		return nil, &PendingBlockCommitBeforeExecutionError{BlockID: b.pendingBlock.ID()}
	}

	// pending block cannot be committed before execution completes
	if b.pendingBlock.ExecutionStarted() && !b.pendingBlock.ExecutionComplete() {
		return nil, &PendingBlockMidExecutionError{BlockID: b.pendingBlock.ID()}
	}

	block := b.pendingBlock.Block()
	collections := b.pendingBlock.Collections()
	transactions := b.pendingBlock.Transactions()
	transactionResults, err := convertToSealedResults(b.pendingBlock.TransactionResults())
	if err != nil {
		return nil, err
	}
	ledgerDelta := b.pendingBlock.LedgerDelta()
	events := b.pendingBlock.Events()

	// commit the pending block to storage
	err = b.storage.CommitBlock(*block, collections, transactions, transactionResults, ledgerDelta, events)
	if err != nil {
		return nil, err
	}

	ledgerView := b.storage.LedgerViewByHeight(block.Header.Height)

	// reset pending block using current block and ledger state
	b.pendingBlock = newPendingBlock(block, ledgerView)

	return block, nil
}

// ExecuteAndCommitBlock is a utility that combines ExecuteBlock with CommitBlock.
func (b *Blockchain) ExecuteAndCommitBlock() (*flowgo.Block, []*types.TransactionResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.executeAndCommitBlock()
}

// ExecuteAndCommitBlock is a utility that combines ExecuteBlock with CommitBlock.
func (b *Blockchain) executeAndCommitBlock() (*flowgo.Block, []*types.TransactionResult, error) {

	results, err := b.executeBlock()
	if err != nil {
		return nil, nil, err
	}

	block, err := b.commitBlock()
	if err != nil {
		return nil, results, err
	}

	return block, results, nil
}

// ResetPendingBlock clears the transactions in pending block.
func (b *Blockchain) ResetPendingBlock() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	latestBlock, err := b.storage.LatestBlock()
	if err != nil {
		return &StorageError{err}
	}

	latestLedgerView := b.storage.LedgerViewByHeight(latestBlock.Header.Height)

	// reset pending block using latest committed block and ledger state
	b.pendingBlock = newPendingBlock(&latestBlock, latestLedgerView)

	return nil
}

// ExecuteScript executes a read-only script against the world state and returns the result.
func (b *Blockchain) ExecuteScript(script []byte, arguments [][]byte) (*types.ScriptResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	latestBlock, err := b.GetLatestBlock()
	if err != nil {
		return nil, err
	}

	return b.ExecuteScriptAtBlock(script, arguments, latestBlock.Header.Height)
}

func (b *Blockchain) ExecuteScriptAtBlock(script []byte, arguments [][]byte, blockHeight uint64) (*types.ScriptResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	requestedBlock, err := b.getBlockByHeight(blockHeight)
	if err != nil {
		return nil, err
	}

	requestedLedgerView := b.storage.LedgerViewByHeight(requestedBlock.Header.Height)

	header := requestedBlock.Header

	blockContext := fvm.NewContextFromParent(
		b.vmCtx,
		fvm.WithBlockHeader(header),
	)

	scriptProc := fvm.Script(script).WithArguments(arguments...)

	err = b.vm.Run(blockContext, scriptProc, requestedLedgerView, programs.NewEmptyPrograms())
	if err != nil {
		return nil, err
	}

	hasher := hash.NewSHA3_256()
	scriptID := sdk.HashToID(hasher.ComputeHash(script))

	events, err := sdkconvert.FlowEventsToSDK(scriptProc.Events)
	if err != nil {
		return nil, err
	}

	var scriptError error = nil
	var convertedValue cadence.Value = nil

	if scriptProc.Err == nil {
		convertedValue = scriptProc.Value
	} else {
		scriptError = convert.VMErrorToEmulator(scriptProc.Err)
	}

	return &types.ScriptResult{
		ScriptID: scriptID,
		Value:    convertedValue,
		Error:    scriptError,
		Logs:     scriptProc.Logs,
		Events:   events,
	}, nil
}

// CreateAccount submits a transaction to create a new account with the given
// account keys and contracts. The transaction is paid by the service account.
func (b *Blockchain) CreateAccount(publicKeys []*sdk.AccountKey, contracts []templates.Contract) (sdk.Address, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	serviceKey := b.ServiceKey()
	serviceAddress := serviceKey.Address

	latestBlock, err := b.GetLatestBlock()
	if err != nil {
		return sdk.Address{}, err
	}

	tx := templates.CreateAccount(publicKeys, contracts, serviceAddress)

	tx.SetGasLimit(flowgo.DefaultMaxTransactionGasLimit).
		SetReferenceBlockID(sdk.Identifier(latestBlock.ID())).
		SetProposalKey(serviceAddress, serviceKey.Index, serviceKey.SequenceNumber).
		SetPayer(serviceAddress)

	err = tx.SignEnvelope(serviceAddress, serviceKey.Index, serviceKey.Signer())
	if err != nil {
		return sdk.Address{}, err
	}

	err = b.addTransaction(*tx)
	if err != nil {
		return sdk.Address{}, err
	}

	_, results, err := b.executeAndCommitBlock()
	if err != nil {
		return sdk.Address{}, err
	}

	lastResult := results[len(results)-1]

	_, err = b.commitBlock()
	if err != nil {
		return sdk.Address{}, err
	}

	if !lastResult.Succeeded() {
		return sdk.Address{}, lastResult.Error
	}

	var address sdk.Address

	for _, event := range lastResult.Events {
		if event.Type == sdk.EventAccountCreated {
			address = sdk.Address(event.Value.Fields[0].(cadence.Address))
			break
		}
	}

	if address == (sdk.Address{}) {
		return sdk.Address{}, fmt.Errorf("failed to find AccountCreated event")
	}

	return address, nil
}

func convertToSealedResults(
	results map[flowgo.Identifier]IndexedTransactionResult,
) (map[flowgo.Identifier]*types.StorableTransactionResult, error) {

	output := make(map[flowgo.Identifier]*types.StorableTransactionResult)

	for id, result := range results {
		temp, err := convert.ToStorableResult(result.Transaction)
		if err != nil {
			return nil, err
		}
		output[id] = &temp
	}

	return output, nil
}

// debugSignatureError tries to unwrap error to the root and test for invalid hashing algorithms
func (b *Blockchain) debugSignatureError(err error, tx *flowgo.TransactionBody) *types.TransactionResultDebug {
	var sigErr *fvmerrors.InvalidProposalSignatureError
	if !errors.As(err, &sigErr) {
		return nil
	}

	switch errors.Unwrap(sigErr).(type) {
	case *fvmerrors.InvalidEnvelopeSignatureError:
		for _, sig := range tx.EnvelopeSignatures {
			debug := b.testAlternativeHashAlgo(sig, tx.EnvelopeMessage())
			if debug != nil {
				return debug
			}
		}
	case *fvmerrors.InvalidPayloadSignatureError:
		for _, sig := range tx.PayloadSignatures {
			debug := b.testAlternativeHashAlgo(sig, tx.PayloadMessage())
			if debug != nil {
				return debug
			}
		}
	}

	return types.NewTransactionInvalidSignature(tx)
}

// testAlternativeHashAlgo tries to verify the signature with alternative hashing algorithm and if
// the signature is verified returns more verbose error
func (b *Blockchain) testAlternativeHashAlgo(sig flowgo.TransactionSignature, msg []byte) *types.TransactionResultDebug {
	acc, err := b.getAccount(sig.Address)
	if err != nil {
		return nil
	}

	key := acc.Keys[sig.KeyIndex]

	for _, algo := range []hash.HashingAlgorithm{sdkcrypto.SHA2_256, sdkcrypto.SHA3_256} {
		if key.HashAlgo == algo {
			continue // skip valid hash algo
		}

		h, _ := fvmcrypto.NewPrefixedHashing(algo, flowgo.TransactionTagString)
		valid, _ := key.PublicKey.Verify(sig.Signature, msg, h)
		if valid {
			return types.NewTransactionInvalidHashAlgo(key, acc.Address, algo)
		}
	}

	return nil
}
