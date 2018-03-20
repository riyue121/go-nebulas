// Copyright (C) 2017 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//

package state

import (
	"encoding/json"
	"sync"

	"github.com/nebulasio/go-nebulas/util/logging"

	"github.com/nebulasio/go-nebulas/consensus/pb"
	"github.com/nebulasio/go-nebulas/util"

	"github.com/nebulasio/go-nebulas/common/mvccdb"
	"github.com/nebulasio/go-nebulas/common/trie"
	"github.com/nebulasio/go-nebulas/storage"
	"github.com/nebulasio/go-nebulas/util/byteutils"
)

func newChangeLog() (*mvccdb.MVCCDB, error) {
	mem, err := storage.NewMemoryStorage()
	if err != nil {
		return nil, err
	}
	db, err := mvccdb.NewMVCCDB(mem, false)
	if err != nil {
		return nil, err
	}

	db.SetStrictGlobalVersionCheck(true)
	return db, nil
}

func newStorage(storage storage.Storage) (*mvccdb.MVCCDB, error) {
	return mvccdb.NewMVCCDB(storage, true)
}

type states struct {
	accState       AccountState
	txsState       *trie.Trie
	eventsState    *trie.Trie
	consensusState ConsensusState

	consensus Consensus
	changelog *mvccdb.MVCCDB
	storage   *mvccdb.MVCCDB
	db        storage.Storage
	txid      interface{}

	gasConsumed map[string]*util.Uint128
	events      map[string][]*Event
}

func newStates(consensus Consensus, stor storage.Storage) (*states, error) {
	changelog, err := newChangeLog()
	if err != nil {
		return nil, err
	}
	storage, err := newStorage(stor)
	if err != nil {
		return nil, err
	}

	accState, err := NewAccountState(nil, storage, false)
	if err != nil {
		return nil, err
	}
	txsState, err := trie.NewTrie(nil, storage, false)
	if err != nil {
		return nil, err
	}
	eventsState, err := trie.NewTrie(nil, storage, false)
	if err != nil {
		return nil, err
	}
	consensusState, err := consensus.NewState(&consensuspb.ConsensusRoot{}, storage, false)
	if err != nil {
		return nil, err
	}

	return &states{
		accState:       accState,
		txsState:       txsState,
		eventsState:    eventsState,
		consensusState: consensusState,

		consensus: consensus,
		changelog: changelog,
		storage:   storage,
		db:        stor,
		txid:      nil,

		gasConsumed: make(map[string]*util.Uint128),
		events:      make(map[string][]*Event),
	}, nil
}

func (s *states) Replay(done *states) error {

	err := s.accState.Replay(done.accState)
	if err != nil {
		return err
	}
	//reply event
	err = s.ReplayEvent(done)
	if err != nil {
		return err
	}
	_, err = s.txsState.Replay(done.txsState)
	if err != nil {
		return err
	}
	err = s.consensusState.Replay(done.consensusState)
	if err != nil {
		return err
	}

	// replay gasconsumed
	for from, gas := range done.gasConsumed {
		consumed, ok := s.gasConsumed[from]
		if !ok {
			consumed = util.NewUint128()
		}
		var err error
		s.gasConsumed[from], err = consumed.Add(gas)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *states) ReplayEvent(done *states) error {

	tx := done.txid.(string)
	events, ok := done.events[tx]
	if !ok {
		return nil
	}

	//replay event
	txHash, err := byteutils.FromHex(tx)
	if err != nil {
		return err
	}
	for idx, event := range events {
		cnt := int64(idx + 1)

		key := append(txHash, byteutils.FromInt64(cnt)...)
		bytes, err := json.Marshal(event)
		if err != nil {
			return err
		}

		_, err = s.eventsState.Put(key, bytes)
		if err != nil {
			return err
		}
	}
	//s.events[tx] = done.events[tx]
	done.events = make(map[string][]*Event)
	return nil
}

func (s *states) Clone() (WorldState, error) {
	changelog, err := newChangeLog()
	if err != nil {
		return nil, err
	}
	storage, err := newStorage(s.db)
	if err != nil {
		return nil, err
	}

	accState, err := s.accState.CopyTo(storage, false)
	if err != nil {
		return nil, err
	}
	txsState, err := s.txsState.CopyTo(storage, false)
	if err != nil {
		return nil, err
	}
	eventsState, err := s.eventsState.CopyTo(storage, false)
	if err != nil {
		return nil, err
	}
	consensusState, err := s.consensusState.CopyTo(storage, false)
	if err != nil {
		return nil, err
	}

	return &states{
		accState:       accState,
		txsState:       txsState,
		eventsState:    eventsState,
		consensusState: consensusState,

		consensus: s.consensus,
		changelog: changelog,
		storage:   storage,
		db:        s.db,
		txid:      s.txid,

		gasConsumed: make(map[string]*util.Uint128),
		events:      make(map[string][]*Event),
	}, nil
}

func (s *states) Begin() error {
	if err := s.changelog.Begin(); err != nil {
		return err
	}
	return s.storage.Begin()
}

func (s *states) Commit() error {
	if err := s.changelog.RollBack(); err != nil {
		return err
	}
	if err := s.storage.Commit(); err != nil {
		return err
	}
	s.events = make(map[string][]*Event)
	s.gasConsumed = make(map[string]*util.Uint128)
	s.accState.CommitAccounts()
	return nil
}

func (s *states) RollBack() error {
	if err := s.changelog.RollBack(); err != nil {
		return err
	}
	if err := s.storage.RollBack(); err != nil {
		return err
	}
	s.events = make(map[string][]*Event)
	s.gasConsumed = make(map[string]*util.Uint128)
	s.accState.RollBackAccounts()
	return nil
}

func (s *states) Prepare(txid interface{}) (TxWorldState, error) {
	changelog, err := s.changelog.Prepare(txid)
	if err != nil {
		return nil, err
	}
	storage, err := s.storage.Prepare(txid)
	if err != nil {
		return nil, err
	}

	accState, err := s.accState.CopyTo(storage, true)
	if err != nil {
		return nil, err
	}
	txsState, err := s.txsState.CopyTo(storage, true)
	if err != nil {
		return nil, err
	}
	eventsState, err := s.eventsState.CopyTo(storage, true)
	if err != nil {
		return nil, err
	}
	consensusState, err := s.consensusState.CopyTo(storage, true)
	if err != nil {
		return nil, err
	}

	return &states{
		accState:       accState,
		txsState:       txsState,
		eventsState:    eventsState,
		consensusState: consensusState,

		consensus: s.consensus,
		changelog: changelog,
		storage:   storage,
		db:        s.db,
		txid:      txid,

		gasConsumed: make(map[string]*util.Uint128),
		events:      make(map[string][]*Event),
	}, nil
}

func (s *states) recordAccounts() error {
	accounts, err := s.accState.DirtyAccounts()
	if err != nil {
		logging.VLog().Info("RAE 1")
		return err
	}
	// record change log
	for _, account := range accounts {
		bytes, err := account.ToBytes()
		if err != nil {
			logging.VLog().Info("RAE 2")
			return err
		}
		if err := s.changelog.Put(account.Address(), bytes); err != nil {
			logging.VLog().Info("RAE 3")
			return err
		}
	}
	return nil
}

func (s *states) CheckAndUpdate(txid interface{}) ([]interface{}, error) {
	if err := s.recordAccounts(); err != nil {
		return nil, err
	}
	dependency, err := s.changelog.CheckAndUpdate()
	if err != nil {
		logging.VLog().Info("CUE 11")
		return nil, err
	}
	_, err = s.storage.CheckAndUpdate()
	if err != nil {
		logging.VLog().Info("CUE 12")
		return nil, err
	}
	return dependency, nil
}

func (s *states) Reset(txid interface{}) error {
	if err := s.changelog.Reset(); err != nil {
		logging.VLog().Info("RSE 11")
		return err
	}
	if err := s.storage.Reset(); err != nil {
		logging.VLog().Info("RSE 12")
		return err
	}
	return nil
}

func (s *states) Close(txid interface{}) error {
	if err := s.changelog.Close(); err != nil {
		logging.VLog().Info("CSE 11")
		return err
	}
	if err := s.storage.Close(); err != nil {
		logging.VLog().Info("CSE 12")
		return err
	}

	return nil
}

func (s *states) AccountsRoot() (byteutils.Hash, error) {
	return s.accState.RootHash()
}

func (s *states) TxsRoot() (byteutils.Hash, error) {
	return s.txsState.RootHash(), nil
}

func (s *states) EventsRoot() (byteutils.Hash, error) {
	return s.eventsState.RootHash(), nil
}

func (s *states) ConsensusRoot() (*consensuspb.ConsensusRoot, error) {
	return s.consensusState.RootHash()
}

func (s *states) GetOrCreateUserAccount(addr byteutils.Hash) (Account, error) {
	return s.accState.GetOrCreateUserAccount(addr)
}

func (s *states) GetContractAccount(addr byteutils.Hash) (Account, error) {
	return s.accState.GetContractAccount(addr)
}

func (s *states) CreateContractAccount(owner byteutils.Hash, birthPlace byteutils.Hash) (Account, error) {
	return s.accState.CreateContractAccount(owner, birthPlace)
}

func (s *states) GetTx(txHash byteutils.Hash) ([]byte, error) {
	bytes, err := s.txsState.Get(txHash)
	if err != nil {
		logging.VLog().Info("GTE 11")
		return nil, err
	}
	// record change log
	if _, err := s.changelog.Get(txHash); err != nil && err != storage.ErrKeyNotFound {
		logging.VLog().Info("GTE 12")
		return nil, err
	}
	return bytes, nil
}

func (s *states) PutTx(txHash byteutils.Hash, txBytes []byte) error {
	_, err := s.txsState.Put(txHash, txBytes)
	if err != nil {
		logging.VLog().Info("PTE 11")
		return err
	}
	// record change log
	if err := s.changelog.Put(txHash, txBytes); err != nil {
		logging.VLog().Info("PTE 12")
		return err
	}
	return nil
}

func (s *states) RecordEvent(txHash byteutils.Hash, event *Event) error {

	events, ok := s.events[txHash.String()]
	if !ok {
		events = make([]*Event, 0)
	}

	cnt := int64(len(s.events) + 1)

	key := append(txHash, byteutils.FromInt64(cnt)...)
	bytes, err := json.Marshal(event)
	if err != nil {
		logging.VLog().Info("REE 11")
		return err
	}

	s.events[txHash.String()] = append(events, event)

	// record change log
	if err := s.changelog.Put(key, bytes); err != nil {
		logging.VLog().Info("REE 12")
		return err
	}
	return nil
}

func (s *states) fetchCacheEvents(txHash byteutils.Hash) ([]*Event, error) {
	txevents, ok := s.events[txHash.String()]
	if !ok {
		return nil, nil
	}

	events := []*Event{}
	for _, event := range txevents {
		events = append(events, event)
	}

	return events, nil
}

func (s *states) FetchEvents(txHash byteutils.Hash) ([]*Event, error) {
	events := []*Event{}
	iter, err := s.eventsState.Iterator(txHash)
	if err != nil && err != storage.ErrKeyNotFound {
		return nil, err
	}
	if err != storage.ErrKeyNotFound {
		exist, err := iter.Next()
		if err != nil {
			logging.VLog().Info("FEE 11")
			return nil, err
		}
		for exist {
			event := new(Event)
			err = json.Unmarshal(iter.Value(), event)
			if err != nil {
				logging.VLog().Info("FEE 12")
				return nil, err
			}
			events = append(events, event)
			// record change log
			if _, err := s.changelog.Get(iter.Key()); err != nil && err != storage.ErrKeyNotFound {
				logging.VLog().Info("FEE 13")
				return nil, err
			}
			exist, err = iter.Next()
			if err != nil {
				logging.VLog().Info("FEE 14")
				return nil, err
			}
		}
	}
	return events, nil
}

func (s *states) Dynasty() ([]byteutils.Hash, error) {
	return s.consensusState.Dynasty()
}

func (s *states) DynastyRoot() byteutils.Hash {
	return s.consensusState.DynastyRoot()
}

func (s *states) Accounts() ([]Account, error) {
	return s.accState.Accounts()
}

func (s *states) LoadAccountsRoot(root byteutils.Hash) error {
	accState, err := NewAccountState(root, s.storage, false)
	if err != nil {
		return err
	}
	s.accState = accState
	return nil
}

func (s *states) LoadTxsRoot(root byteutils.Hash) error {
	txsState, err := trie.NewTrie(root, s.storage, false)
	if err != nil {
		return err
	}
	s.txsState = txsState
	return nil
}

func (s *states) LoadEventsRoot(root byteutils.Hash) error {
	eventsState, err := trie.NewTrie(root, s.storage, false)
	if err != nil {
		return err
	}
	s.eventsState = eventsState
	return nil
}

func (s *states) LoadConsensusRoot(root *consensuspb.ConsensusRoot) error {
	consensusState, err := s.consensus.NewState(root, s.storage, false)
	if err != nil {
		return err
	}
	s.consensusState = consensusState
	return nil
}

func (s *states) NextConsensusState(elapsedSecond int64) (ConsensusState, error) {
	return s.consensusState.NextConsensusState(elapsedSecond, s)
}

func (s *states) SetConsensusState(consensusState ConsensusState) {
	s.consensusState = consensusState
}

func (s *states) RecordGas(from string, gas *util.Uint128) error {
	consumed, ok := s.gasConsumed[from]
	if !ok {
		consumed = util.NewUint128()
	}
	var err error
	s.gasConsumed[from], err = consumed.Add(gas)

	return err
}

func (s *states) GetGas() map[string]*util.Uint128 {
	gasConsumed := make(map[string]*util.Uint128)
	for from, gas := range s.gasConsumed {
		gasConsumed[from] = gas
	}
	s.gasConsumed = make(map[string]*util.Uint128)
	return gasConsumed
}

// WorldState manange all current states in Blockchain
type worldState struct {
	*states
	txStates *sync.Map
}

// NewWorldState create a new empty WorldState
func NewWorldState(consensus Consensus, storage storage.Storage) (WorldState, error) {
	states, err := newStates(consensus, storage)
	if err != nil {
		return nil, err
	}
	return &worldState{
		states:   states,
		txStates: new(sync.Map),
	}, nil
}

// Clone a new WorldState
func (ws *worldState) Clone() (WorldState, error) {
	s, err := ws.states.Clone()
	if err != nil {
		return nil, err
	}
	return &worldState{
		states:   s.(*states),
		txStates: new(sync.Map),
	}, nil
}

func (ws *worldState) Begin() error {
	if err := ws.states.Begin(); err != nil {
		return err
	}
	return nil
}

func (ws *worldState) Commit() error {
	if err := ws.states.Commit(); err != nil {
		ws.Dispose()
		return err
	}
	ws.Dispose()
	return nil
}

func (ws *worldState) RollBack() error {
	if err := ws.states.RollBack(); err != nil {
		ws.Dispose()
		return err
	}
	ws.Dispose()
	return nil
}

func (ws *worldState) Dispose() {
	ws.txStates = new(sync.Map)
}

type txWorldState struct {
	*states
	txid interface{}
}

func (ws *worldState) Prepare(txid interface{}) (TxWorldState, error) {
	if _, ok := ws.txStates.Load(txid); ok {
		return nil, ErrCannotPrepareTxStateTwice
	}
	s, err := ws.states.Prepare(txid)
	if err != nil {
		logging.VLog().Info("PPE 1")
		return nil, err
	}
	txState := &txWorldState{
		states: s.(*states),
		txid:   txid,
	}
	ws.txStates.Store(txid, txState)
	return txState, nil
}

func (ws *worldState) CheckAndUpdate(txid interface{}) ([]interface{}, error) {
	state, ok := ws.txStates.Load(txid)
	if !ok {
		return nil, ErrCannotUpdateTxStateBeforePrepare
	}
	txWorldState := state.(*txWorldState)
	dependencies, err := txWorldState.CheckAndUpdate(txid)
	if err != nil {
		logging.VLog().Info("CUE 1")
		return nil, err
	}
	if err := ws.states.Replay(txWorldState.states); err != nil {
		logging.VLog().Info("CUE 2")
		return nil, err
	}

	return dependencies, nil
}

func (ws *worldState) Reset(txid interface{}) error {
	state, ok := ws.txStates.Load(txid)
	if !ok {
		return ErrCannotUpdateTxStateBeforePrepare
	}
	txWorldState := state.(*txWorldState)
	if err := txWorldState.Reset(txid); err != nil {
		logging.VLog().Info("RSE 1")
		return err
	}
	return nil
}

func (ws *worldState) Close(txid interface{}) error {
	state, ok := ws.txStates.Load(txid)
	if !ok {
		return ErrCannotUpdateTxStateBeforePrepare
	}
	txWorldState := state.(*txWorldState)
	if err := txWorldState.Close(txid); err != nil {
		logging.VLog().Info("CSE 1")
		return err
	}
	ws.txStates.Delete(txid)
	return nil
}

func (ts *txWorldState) TxID() interface{} {
	return ts.txid
}
