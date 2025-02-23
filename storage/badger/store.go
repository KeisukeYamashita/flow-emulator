/*
 * Flow Emulator
 *
 * Copyright 2019-2022 Dapper Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v2"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"
	"github.com/onflow/flow-emulator/storage"
	"github.com/onflow/flow-emulator/types"
	"github.com/onflow/flow-go/engine/execution/state/delta"
	flowgo "github.com/onflow/flow-go/model/flow"
)

// Store is an embedded storage implementation using Badger as the underlying
// persistent key-value store.
type Store struct {
	db              *badger.DB
	ledgerChangeLog changelog
	dbGitRepository *git.Repository
	path            string
	badgerOptions   badger.Options
}

var _ storage.Store = &Store{}

func getTag(r *git.Repository, tag string) *object.Tag {
	tags, err := r.TagObjects()
	if err != nil {
		return nil
	}
	var res *object.Tag = nil
	_ = tags.ForEach(func(t *object.Tag) error {
		if t.Name == tag {
			res = t
		}
		return nil
	})
	return res
}

func setTag(r *git.Repository, tag string, tagger *object.Signature) (bool, error) {
	if getTag(r, tag) != nil {
		return false, nil
	}
	h, err := r.Head()
	if err != nil {
		return false, err
	}
	_, err = r.CreateTag(tag, h.Hash(), &git.CreateTagOptions{
		Tagger:  tagger,
		Message: tag,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}
func defaultSignature(name, email string) *object.Signature {
	return &object.Signature{
		Name:  name,
		Email: email,
		When:  time.Now(),
	}
}

//prevents git commits when emulator running
//ignoring error here but it is not critical to operation
func (s *Store) lockGit() {
	lockPath := fmt.Sprintf("%s/.git/index.lock", s.path)
	_ = ioutil.WriteFile(lockPath, []byte("emulatorLock"), 0755)
}

//ignoring error here but it is not critical to operation
func (s *Store) unlockGit() {
	lockPath := fmt.Sprintf("%s/.git/index.lock", s.path)
	_ = os.Remove(lockPath)
}

func (s *Store) JumpToContext(context string) error {
	s.unlockGit()
	defer s.lockGit()
	err := s.db.Close()
	if err != nil {
		return err
	}

	err = s.newCommit(fmt.Sprintf("Context switching to: %s", context))
	if err != nil {
		return err
	}
	w, err := s.dbGitRepository.Worktree()
	if err != nil {
		return err
	}
	branch := fmt.Sprintf("refs/heads/%s", context)
	b := plumbing.ReferenceName(branch)

	// checkout branch ( first branch name is actually context name )
	err = w.Checkout(&git.CheckoutOptions{Create: false, Force: true, Branch: b})

	if err != nil {
		// branch doesn't exist, it means we need to create it ( first branch is named after context )
		err := w.Checkout(&git.CheckoutOptions{Create: true, Force: true, Branch: b})
		if err != nil {
			return err
		}

		//after we create a tag pointing to start of this context
		created, err := setTag(s.dbGitRepository, context, defaultSignature("Emulator", "emulator@onflow.org"))
		if err != nil && !created {
			return err
		}

		s.badgerOptions.Logger.Infof("Created a new state snapshot with the name '%s'", context)

	} else {

		//create new branch
		uuidWithHyphen := uuid.New()
		newBranchUuid := strings.Replace(uuidWithHyphen.String(), "-", "", -1)

		err := w.Checkout(&git.CheckoutOptions{Create: true, Force: true, Branch: plumbing.NewBranchReferenceName(newBranchUuid)})
		if err != nil {
			return err
		}

		//we have new branch but we don't need to create a tag, we just need to reset to tag
		tag := getTag(s.dbGitRepository, context)
		if tag != nil && !tag.Hash.IsZero() {
			commit, _ := tag.Commit()
			_ = w.Reset(&git.ResetOptions{
				Mode:   git.HardReset,
				Commit: commit.Hash,
			})
		}
		s.badgerOptions.Logger.Infof("Switched to snapshot with name '%s'", context)

	}

	s.db, err = badger.Open(s.badgerOptions)
	if err != nil {
		return fmt.Errorf("could not open database: %w", err)
	}

	return nil

}

func (s *Store) newCommit(message string) error {
	s.unlockGit()
	defer s.lockGit()
	err := s.Sync()
	if err != nil {
		return err
	}

	w, err := s.dbGitRepository.Worktree()
	if err != nil {
		return err
	}

	err = filepath.Walk(s.path, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}

		if info.Name() == "KEYREGISTRY" || info.Name() == "MANIFEST" || info.Name() == "LOCK" {
			_, adderr := w.Add(path[strings.LastIndex(path, "/")+1:])
			return adderr
		}

		if filepath.Ext(path) == ".vlog" || filepath.Ext(path) == ".sst" {
			_, adderr := w.Add(path[strings.LastIndex(path, "/")+1:])
			return adderr
		}
		return nil
	})

	if err != nil {
		return err
	}

	_, err = w.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Flow Emulator",
			Email: "emulator@onflow.org",
			When:  time.Now(),
		},
	})

	if err != nil {
		return err
	}
	return nil
}

func (s *Store) openRepository(directory string) (*git.Repository, error) {
	dbgit, err := git.PlainOpen(directory)
	if err == nil {
		return dbgit, err
	}
	if err == git.ErrRepositoryNotExists {
		result, err := git.PlainInit(directory, false)
		if err == nil {
			return result, err
		}
		return nil, err
	}
	return nil, err
}

// New returns a new Badger Store.
func New(opts ...Opt) (*Store, error) {
	badgerOptions := getBadgerOptions(opts...)
	badgerOptions.BypassLockGuard = true
	db, err := badger.Open(badgerOptions)
	if err != nil {
		return nil, fmt.Errorf("could not open database: %w", err)
	}
	_ = db.Sync()

	store := &Store{db, newChangelog(), nil, badgerOptions.Dir, badgerOptions}
	if err = store.setup(); err != nil {
		return nil, err
	}

	return store, nil
}

// setups git, setup sets up in-memory indexes and prepares the store for use.
func (s *Store) setup() error {

	dbgit, err := s.openRepository(s.path)
	s.dbGitRepository = dbgit
	if err != nil {
		return err
	}

	w, _ := dbgit.Worktree()
	r, _ := dbgit.Head()
	if r != nil {
		_ = w.Reset(&git.ResetOptions{
			Mode:   git.HardReset,
			Commit: r.Hash(),
		})
	}
	err = s.newCommit("Emulator Started New Session")
	if err != nil {
		return err
	}
	s.lockGit()

	s.db.RLock()
	defer s.db.RUnlock()

	iterOpts := badger.DefaultIteratorOptions
	// only search for changelog entries
	iterOpts.Prefix = []byte(ledgerChangelogKeyPrefix)
	// create a buffer for copying changelists, this is reused for each register
	clistBuf := make([]byte, 256)

	// read the changelist from disk for each register
	return s.db.View(func(txn *badger.Txn) error {
		iter := txn.NewIterator(iterOpts)
		defer iter.Close()

		for iter.Rewind(); iter.Valid(); iter.Next() {
			item := iter.Item()
			registerID, err := registerIDFromLedgerChangelogKey(item.Key())
			// ensure the register ID is value
			if err != nil {
				return errors.New("found changelist for invalid register ID")
			}

			// decode the changelist
			encClist, err := item.ValueCopy(clistBuf)
			if err != nil {
				return err
			}
			var clist changelist
			if err := decodeChangelist(&clist, encClist); err != nil {
				return err
			}

			// add to the changelog
			s.ledgerChangeLog.setChangelist(registerID, clist)
		}
		return nil
	})
}

func (s *Store) LatestBlock() (block flowgo.Block, err error) {
	err = s.db.View(func(txn *badger.Txn) error {
		// get latest block height
		latestBlockHeight, err := getLatestBlockHeightTx(txn)
		if err != nil {
			return err
		}

		// get corresponding block
		encBlock, err := getTx(txn)(blockKey(latestBlockHeight))
		if err != nil {
			return err
		}
		return decodeBlock(&block, encBlock)
	})
	return
}

func (s *Store) BlockByID(blockID flowgo.Identifier) (block *flowgo.Block, err error) {
	err = s.db.View(func(txn *badger.Txn) error {
		// get block height by block ID
		encBlockHeight, err := getTx(txn)(blockIDIndexKey(blockID))
		if err != nil {
			return err
		}

		// decode block height
		var blockHeight uint64
		if err := decodeUint64(&blockHeight, encBlockHeight); err != nil {
			return err
		}

		// get block by block height and decode
		encBlock, err := getTx(txn)(blockKey(blockHeight))
		if err != nil {
			return err
		}
		block = &flowgo.Block{}
		return decodeBlock(block, encBlock)
	})
	return
}

func (s *Store) BlockByHeight(blockHeight uint64) (block *flowgo.Block, err error) {
	err = s.db.View(func(txn *badger.Txn) error {
		encBlock, err := getTx(txn)(blockKey(blockHeight))
		if err != nil {
			return err
		}
		block = &flowgo.Block{}
		return decodeBlock(block, encBlock)
	})
	return
}

func (s *Store) StoreBlock(block *flowgo.Block) error {
	return s.db.Update(store(block))
}

func store(block *flowgo.Block) func(txn *badger.Txn) error {
	return func(txn *badger.Txn) error {
		encBlock, err := encodeBlock(*block)
		if err != nil {
			return err
		}
		encBlockHeight, err := encodeUint64(block.Header.Height)
		if err != nil {
			return err
		}

		// get latest block height
		latestBlockHeight, err := getLatestBlockHeightTx(txn)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}

		// insert the block by block height
		if err := txn.Set(blockKey(block.Header.Height), encBlock); err != nil {
			return err
		}
		// add block ID to ID->height lookup
		if err := txn.Set(blockIDIndexKey(block.ID()), encBlockHeight); err != nil {
			return err
		}

		// if this is latest block, set latest block
		if block.Header.Height >= latestBlockHeight {
			return txn.Set(latestBlockKey(), encBlockHeight)
		}

		return nil
	}
}

func (s *Store) CommitBlock(
	block flowgo.Block,
	collections []*flowgo.LightCollection,
	transactions map[flowgo.Identifier]*flowgo.TransactionBody,
	transactionResults map[flowgo.Identifier]*types.StorableTransactionResult,
	delta delta.Delta,
	events []flowgo.Event,
) error {
	if len(transactions) != len(transactionResults) {
		return fmt.Errorf(
			"transactions count (%d) does not match result count (%d)",
			len(transactions),
			len(transactionResults),
		)
	}

	message := fmt.Sprintf("Committed Block: %s\n", block.ID().String())

	err := s.db.Update(func(txn *badger.Txn) error {
		err := store(&block)(txn)
		if err != nil {
			return err
		}

		for _, col := range collections {
			err := insertCollection(*col)(txn)
			if err != nil {
				return err
			}
		}

		for txID, tx := range transactions {
			err := insertTransaction(txID, *tx)(txn)
			if err != nil {
				return err
			}

			message = fmt.Sprintf("%sTransaction    : %s\n\n", message, txID.String())

			message = fmt.Sprintf("%sArguments (%d): \n\n", message, len(tx.Arguments))
			for argID, arg := range tx.Arguments {
				message = fmt.Sprintf("%s\t- Argument %d: %s\n", message, argID, string(arg))
			}

			message = fmt.Sprintf("%sCode:\n%s", message, tx.Script)

			result := transactionResults[txID]
			err = insertTransactionResult(txID, *result)(txn)
			if err != nil {
				return err
			}

			message = fmt.Sprintf("%sResult:\n\n", message)

			message = fmt.Sprintf("%s\t- Error Message : [%d] %s\n\n", message, result.ErrorCode, result.ErrorMessage)
			message = fmt.Sprintf("%s\t- Logs (%d): \n\n", message, len(result.Logs))

			for _, log := range result.Logs {
				message = fmt.Sprintf("%s\t\t+ %s\n", message, log)
			}

			message = fmt.Sprintf("%s\t- Events (%d): \n\n", message, len(result.Events))

			for _, event := range result.Events {
				message = fmt.Sprintf("%s\t\t+ %d - %s - %s \n", message, event.EventIndex, event.Type, "")
			}

		}

		err = s.insertLedgerDelta(block.Header.Height, delta)(txn)
		if err != nil {
			return err
		}

		if events != nil {
			err = insertEvents(block.Header.Height, events)(txn)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return err
	}
	err = s.newCommit(message)
	return err
}

func (s *Store) CollectionByID(colID flowgo.Identifier) (col flowgo.LightCollection, err error) {
	err = s.db.View(func(txn *badger.Txn) error {
		encCol, err := getTx(txn)(collectionKey(colID))
		if err != nil {
			return err
		}
		return decodeCollection(&col, encCol)
	})
	return
}

func (s *Store) InsertCollection(col flowgo.LightCollection) error {
	return s.db.Update(insertCollection(col))
}

func insertCollection(col flowgo.LightCollection) func(txn *badger.Txn) error {
	return func(txn *badger.Txn) error {
		encCol, err := encodeCollection(col)
		if err != nil {
			return err
		}

		return txn.Set(collectionKey(col.ID()), encCol)
	}
}

func (s *Store) TransactionByID(txID flowgo.Identifier) (tx flowgo.TransactionBody, err error) {
	err = s.db.View(func(txn *badger.Txn) error {
		encTx, err := getTx(txn)(transactionKey(txID))
		if err != nil {
			return err
		}
		return decodeTransaction(&tx, encTx)
	})
	return
}

func (s *Store) InsertTransaction(tx flowgo.TransactionBody) error {
	return s.db.Update(insertTransaction(tx.ID(), tx))
}

func insertTransaction(txID flowgo.Identifier, tx flowgo.TransactionBody) func(txn *badger.Txn) error {
	return func(txn *badger.Txn) error {
		encTx, err := encodeTransaction(tx)
		if err != nil {
			return err
		}

		return txn.Set(transactionKey(txID), encTx)
	}
}

func (s *Store) TransactionResultByID(txID flowgo.Identifier) (result types.StorableTransactionResult, err error) {
	err = s.db.View(func(txn *badger.Txn) error {
		encResult, err := getTx(txn)(transactionResultKey(txID))
		if err != nil {
			return err
		}
		return decodeTransactionResult(&result, encResult)
	})
	return
}

func (s *Store) InsertTransactionResult(txID flowgo.Identifier, result types.StorableTransactionResult) error {
	return s.db.Update(insertTransactionResult(txID, result))
}

func insertTransactionResult(txID flowgo.Identifier, result types.StorableTransactionResult) func(txn *badger.Txn) error {
	return func(txn *badger.Txn) error {
		encResult, err := encodeTransactionResult(result)
		if err != nil {
			return err
		}

		return txn.Set(transactionResultKey(txID), encResult)
	}
}

func (s *Store) LedgerViewByHeight(blockHeight uint64) *delta.View {
	return delta.NewView(func(owner, controller, key string) (value flowgo.RegisterValue, err error) {
		id := flowgo.RegisterID{
			Owner:      owner,
			Controller: controller,
			Key:        key,
		}

		//return types.NewLedgerView(func(key string) (value []byte, err error) {
		s.ledgerChangeLog.RLock()
		defer s.ledgerChangeLog.RUnlock()

		lastChangedBlock := s.ledgerChangeLog.getMostRecentChange(id, blockHeight)

		err = s.db.View(func(txn *badger.Txn) error {
			value, err = getTx(txn)(ledgerValueKey(id, lastChangedBlock))
			if err != nil {
				return err
			}
			return nil
		})

		if err != nil {
			// silence not found errors
			if errors.Is(err, storage.ErrNotFound) {
				return nil, nil
			}

			return nil, err
		}

		return value, nil
	})
}

func (s *Store) InsertLedgerDelta(blockHeight uint64, delta delta.Delta) error {
	return s.db.Update(s.insertLedgerDelta(blockHeight, delta))
}

func (s *Store) insertLedgerDelta(blockHeight uint64, delta delta.Delta) func(txn *badger.Txn) error {
	return func(txn *badger.Txn) error {
		s.ledgerChangeLog.Lock()
		defer s.ledgerChangeLog.Unlock()

		updatedIDs, updatedValues := delta.RegisterUpdates()
		for i, registerID := range updatedIDs {
			value := updatedValues[i]
			if value != nil {
				// if register has an updated value, write it at this block
				err := txn.Set(ledgerValueKey(registerID, blockHeight), value)
				if err != nil {
					return err
				}
			}

			// otherwise register has been deleted, so record change
			// and keep value as nil

			// update the in-memory changelog
			s.ledgerChangeLog.addChange(registerID, blockHeight)

			// encode and write the changelist for the register to disk
			encChangelist, err := encodeChangelist(s.ledgerChangeLog.getChangelist(registerID))
			if err != nil {
				return err
			}

			if err := txn.Set(ledgerChangelogKey(registerID), encChangelist); err != nil {
				return err
			}
		}
		return nil
	}
}

func (s *Store) EventsByHeight(blockHeight uint64, eventType string) (events []flowgo.Event, err error) {
	// set up an iterator over all events in the block
	iterOpts := badger.DefaultIteratorOptions
	iterOpts.Prefix = eventKeyBlockPrefix(blockHeight)

	eventTypeBytes := []byte(eventType)

	err = s.db.View(func(txn *badger.Txn) error {
		iter := txn.NewIterator(iterOpts)
		defer iter.Close()

		// start from lowest possible event key for this block
		startKey := eventKey(blockHeight, 0, 0, "")

		// iteration happens in byte-wise lexicographical sorting order
		for iter.Seek(startKey); iter.Valid(); iter.Next() {
			item := iter.Item()

			// filter by event type if specified
			if eventType != "" {
				if !eventKeyHasType(item.Key(), eventTypeBytes) {
					continue
				}
			}

			err = item.Value(func(b []byte) error {
				var event flowgo.Event

				err := decodeEvent(&event, b)
				if err != nil {
					return err
				}

				events = append(events, event)

				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	return
}

func (s *Store) InsertEvents(blockHeight uint64, events []flowgo.Event) error {
	return s.db.Update(insertEvents(blockHeight, events))
}

func insertEvents(blockHeight uint64, events []flowgo.Event) func(txn *badger.Txn) error {
	return func(txn *badger.Txn) error {
		for _, event := range events {
			b, err := encodeEvent(event)
			if err != nil {
				return err
			}

			key := eventKey(blockHeight, event.TransactionIndex, event.EventIndex, event.Type)

			err = txn.Set(key, b)
			if err != nil {
				return err
			}
		}

		return nil
	}
}

// Close closes the underlying Badger database. It is necessary to close
// a Store before exiting to ensure all writes are persisted to disk.
func (s *Store) Close() error {
	err := s.db.Close()
	if err != nil {
		return err
	}
	err = s.newCommit("Emulator Ended Session")
	if err != nil {
		return err
	}
	s.unlockGit()
	return err
}

// Sync syncs database content to disk.
func (s *Store) Sync() error {
	return s.db.Sync()
}

func (s *Store) RunValueLogGC(discardRatio float64) error {
	err := s.db.RunValueLogGC(discardRatio)

	// ignore ErrNoRewrite, which occurs when GC results in no cleanup
	if err != nil && !errors.Is(err, badger.ErrNoRewrite) {
		return err
	}

	return nil
}

// getTx returns a getter function bound to the input transaction that can be
// used to get values from Badger.
//
// The getter function checks for key-not-found errors and wraps them in
// storage.NotFound in order to comply with the storage.Store interface.
//
// This saves a few lines of converting a badger.Item to []byte.
func getTx(txn *badger.Txn) func([]byte) ([]byte, error) {
	return func(key []byte) ([]byte, error) {
		// Badger returns an "item" upon GETs, we need to copy the actual value
		// from the item and return it.
		item, err := txn.Get(key)
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return nil, storage.ErrNotFound
			}
			return nil, err
		}

		val := make([]byte, item.ValueSize())
		return item.ValueCopy(val)
	}
}

// getLatestBlockHeightTx retrieves the latest block height and returns it.
// Must be called from within a Badger transaction.
func getLatestBlockHeightTx(txn *badger.Txn) (uint64, error) {
	encBlockHeight, err := getTx(txn)(latestBlockKey())
	if err != nil {
		return 0, err
	}

	var blockHeight uint64
	if err := decodeUint64(&blockHeight, encBlockHeight); err != nil {
		return 0, err
	}

	return blockHeight, nil
}
