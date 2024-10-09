// (c) 2019-2020, Ava Labs, Inc.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package state provides a caching layer atop the Ethereum state trie.
package state

import (
	"math/big"
	"reflect"

	"github.com/ava-labs/coreth/core/state/snapshot"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ethereum/go-ethereum/common"
	gethstate "github.com/ethereum/go-ethereum/core/state"
	"github.com/holiman/uint256"
)

type snapshotTree interface {
	Snapshot(root common.Hash) snapshot.Snapshot
	Update(
		blockRoot common.Hash,
		parentRoot common.Hash,
		destructs map[common.Hash]struct{},
		accounts map[common.Hash][]byte,
		storage map[common.Hash]map[common.Hash][]byte,
	) error
	StorageIterator(root common.Hash, account common.Hash, seek common.Hash) (snapshot.StorageIterator, error)
	Cap(root common.Hash, layers int) error
}

// StateDB structs within the ethereum protocol are used to store anything
// within the merkle trie. StateDBs take care of caching and storing
// nested states. It's the general query interface to retrieve:
//
// * Contracts
// * Accounts
//
// Once the state is committed, tries cached in stateDB (including account
// trie, storage tries) will no longer be functional. A new state instance
// must be created with new root and updated database for accessing post-
// commit states.
type StateDB struct {
	*gethstate.StateDB

	// The tx context
	thash   common.Hash
	txIndex int

	// Some fields remembered as they are used in tests
	db    Database
	snaps snapshotTree
}

// New creates a new state from a given trie.
func New(root common.Hash, db Database, snaps snapshotTree) (*StateDB, error) {
	if snaps != nil {
		// XXX: Make sure we treat incoming `nil` ptrs as `nil` values, not an
		// interface to a nil ptr
		v := reflect.ValueOf(snaps)
		if v.Kind() == reflect.Ptr && v.IsNil() {
			snaps = nil
		}
	}
	stateDB, err := gethstate.New(root, db, snaps)
	if err != nil {
		return nil, err
	}
	return &StateDB{
		StateDB: stateDB,
		db:      db,
		snaps:   snaps,
	}, nil
}

// StartPrefetcher initializes a new trie prefetcher to pull in nodes from the
// state trie concurrently while the state is mutated so that when we reach the
// commit phase, most of the needed data is already hot.
func (s *StateDB) StartPrefetcher(namespace string, maxConcurrency int) {
	s.StateDB.StartPrefetcher(namespace) // XXX: Trie prefetcher parallelism should be added back
}

// Retrieve the balance from the given address or 0 if object not found
func (s *StateDB) GetBalanceMultiCoin(addr common.Address, coinID common.Hash) *big.Int {
	NormalizeCoinID(&coinID)
	return s.StateDB.GetState(addr, coinID).Big()
}

// GetState retrieves a value from the given account's storage trie.
func (s *StateDB) GetState(addr common.Address, hash common.Hash) common.Hash {
	NormalizeStateKey(&hash)
	return s.StateDB.GetState(addr, hash)
}

// AddBalance adds amount to the account associated with addr.
func (s *StateDB) AddBalanceMultiCoin(addr common.Address, coinID common.Hash, amount *big.Int) {
	if amount.Sign() == 0 {
		s.AddBalance(addr, new(uint256.Int)) // used to cause touch
		return
	}
	if !gethstate.GetExtra(s.StateDB, types.IsMultiCoinPayloads, addr) {
		gethstate.SetExtra(s.StateDB, types.IsMultiCoinPayloads, addr, true)
	}
	newAmount := new(big.Int).Add(s.GetBalanceMultiCoin(addr, coinID), amount)
	NormalizeCoinID(&coinID)
	s.StateDB.SetState(addr, coinID, common.BigToHash(newAmount))
}

// SubBalance subtracts amount from the account associated with addr.
func (s *StateDB) SubBalanceMultiCoin(addr common.Address, coinID common.Hash, amount *big.Int) {
	if amount.Sign() == 0 {
		return
	}
	// Note: It's not needed to set the IsMultiCoin (extras) flag here, as this
	// call would always be preceded by a call to AddBalanceMultiCoin, which would
	// set the extra flag. Seems we should remove the redundant code.
	if !gethstate.GetExtra(s.StateDB, types.IsMultiCoinPayloads, addr) {
		gethstate.SetExtra(s.StateDB, types.IsMultiCoinPayloads, addr, true)
	}
	newAmount := new(big.Int).Sub(s.GetBalanceMultiCoin(addr, coinID), amount)
	NormalizeCoinID(&coinID)
	s.StateDB.SetState(addr, coinID, common.BigToHash(newAmount))
}

func (s *StateDB) SetState(addr common.Address, key, value common.Hash) {
	NormalizeStateKey(&key)
	s.StateDB.SetState(addr, key, value)
}

// SetTxContext sets the current transaction hash and index which are
// used when the EVM emits new state logs. It should be invoked before
// transaction execution.
func (s *StateDB) SetTxContext(thash common.Hash, ti int) {
	s.thash = thash
	s.txIndex = ti
	s.StateDB.SetTxContext(thash, ti)
}

// GetTxHash returns the current tx hash on the StateDB set by SetTxContext.
func (s *StateDB) GetTxHash() common.Hash {
	return s.thash
}

func (s *StateDB) Copy() *StateDB {
	return &StateDB{
		StateDB: s.StateDB.Copy(),
		db:      s.db,
		snaps:   s.snaps,
		thash:   s.thash,
		txIndex: s.txIndex,
	}
}

// NormalizeCoinID ORs the 0th bit of the first byte in
// [coinID], which ensures this bit will be 1 and all other
// bits are left the same.
// This partitions multicoin storage from normal state storage.
func NormalizeCoinID(coinID *common.Hash) {
	coinID[0] |= 0x01
}

// NormalizeStateKey ANDs the 0th bit of the first byte in
// [key], which ensures this bit will be 0 and all other bits
// are left the same.
// This partitions normal state storage from multicoin storage.
func NormalizeStateKey(key *common.Hash) {
	key[0] &= 0xfe
}
