// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Ben Darnell

package storage

import (
	"bytes"
	"time"

	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/util/bufalloc"
	"github.com/cockroachdb/cockroach/util/hlc"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/cockroachdb/cockroach/util/protoutil"
	"github.com/cockroachdb/cockroach/util/retry"
	"github.com/cockroachdb/cockroach/util/timeutil"
)

var _ raft.Storage = (*Replica)(nil)

// All calls to raft.RawNode require that an exclusive lock is held.
// All of the functions exposed via the raft.Storage interface will in
// turn be called from RawNode. So the lock that guards raftGroup must
// be the same as the lock that guards all the inner fields.
//
// Many of the methods defined in this file are wrappers around static
// functions. This is done to facilitate their use from
// Replica.Snapshot(), where it is important that all the data that
// goes into the snapshot comes from a consistent view of the
// database, and not the replica's in-memory state or via a reference
// to Replica.store.Engine().

// InitialState implements the raft.Storage interface.
// InitialState requires that the replica lock be held.
func (r *Replica) InitialState() (raftpb.HardState, raftpb.ConfState, error) {
	hs, err := loadHardState(r.store.Engine(), r.RangeID)
	// For uninitialized ranges, membership is unknown at this point.
	if raft.IsEmptyHardState(hs) || err != nil {
		return raftpb.HardState{}, raftpb.ConfState{}, err
	}
	var cs raftpb.ConfState
	for _, rep := range r.mu.state.Desc.Replicas {
		cs.Nodes = append(cs.Nodes, uint64(rep.ReplicaID))
	}

	return hs, cs, nil
}

// Entries implements the raft.Storage interface. Note that maxBytes is advisory
// and this method will always return at least one entry even if it exceeds
// maxBytes. Passing maxBytes equal to zero disables size checking.
// TODO(bdarnell): consider caching for recent entries, if rocksdb's built in
// caching is insufficient.
// Entries requires that the replica lock is held.
func (r *Replica) Entries(lo, hi, maxBytes uint64) ([]raftpb.Entry, error) {
	snap := r.store.NewSnapshot()
	defer snap.Close()
	return entries(snap, r.RangeID, lo, hi, maxBytes)
}

func entries(
	e engine.Reader,
	rangeID roachpb.RangeID,
	lo, hi, maxBytes uint64,
) ([]raftpb.Entry, error) {
	if lo > hi {
		return nil, errors.Errorf("lo:%d is greater than hi:%d", lo, hi)
	}
	// Scan over the log to find the requested entries in the range [lo, hi),
	// stopping once we have enough.
	ents := make([]raftpb.Entry, 0, hi-lo)
	size := uint64(0)
	var ent raftpb.Entry
	expectedIndex := lo
	exceededMaxBytes := false
	scanFunc := func(kv roachpb.KeyValue) (bool, error) {
		if err := kv.Value.GetProto(&ent); err != nil {
			return false, err
		}
		// Exit early if we have any gaps or it has been compacted.
		if ent.Index != expectedIndex {
			return true, nil
		}
		expectedIndex++
		size += uint64(ent.Size())
		ents = append(ents, ent)
		exceededMaxBytes = maxBytes > 0 && size > maxBytes
		return exceededMaxBytes, nil
	}

	if err := iterateEntries(e, rangeID, lo, hi, scanFunc); err != nil {
		return nil, err
	}

	// Did the correct number of results come back? If so, we're all good.
	if uint64(len(ents)) == hi-lo {
		return ents, nil
	}

	// Did we hit the size limit? If so, return what we have.
	if exceededMaxBytes {
		return ents, nil
	}

	// Did we get any results at all? Because something went wrong.
	if len(ents) > 0 {
		// Was the lo already truncated?
		if ents[0].Index > lo {
			return nil, raft.ErrCompacted
		}

		// Was the missing index after the last index?
		lastIndex, err := loadLastIndex(e, rangeID)
		if err != nil {
			return nil, err
		}
		if lastIndex <= expectedIndex {
			return nil, raft.ErrUnavailable
		}

		// We have a gap in the record, if so, return a nasty error.
		return nil, errors.Errorf("there is a gap in the index record between lo:%d and hi:%d at index:%d", lo, hi, expectedIndex)
	}

	// No results, was it due to unavailability or truncation?
	ts, err := raftTruncatedState(e, rangeID)
	if err != nil {
		return nil, err
	}
	if ts.Index >= lo {
		// The requested lo index has already been truncated.
		return nil, raft.ErrCompacted
	}
	// The requested lo index does not yet exist.
	return nil, raft.ErrUnavailable
}

func iterateEntries(
	e engine.Reader, rangeID roachpb.RangeID, lo, hi uint64, scanFunc func(roachpb.KeyValue) (bool, error),
) error {
	_, err := engine.MVCCIterate(
		context.Background(), e,
		keys.RaftLogKey(rangeID, lo),
		keys.RaftLogKey(rangeID, hi),
		hlc.ZeroTimestamp,
		true,  /* consistent */
		nil,   /* txn */
		false, /* !reverse */
		scanFunc,
	)
	return err
}

// Term implements the raft.Storage interface.
// Term requires that the replica lock is held.
func (r *Replica) Term(i uint64) (uint64, error) {
	snap := r.store.NewSnapshot()
	defer snap.Close()
	return term(snap, r.RangeID, i)
}

func term(eng engine.Reader, rangeID roachpb.RangeID, i uint64) (uint64, error) {
	ents, err := entries(eng, rangeID, i, i+1, 0)
	if err == raft.ErrCompacted {
		ts, err := raftTruncatedState(eng, rangeID)
		if err != nil {
			return 0, err
		}
		if i == ts.Index {
			return ts.Term, nil
		}
		return 0, raft.ErrCompacted
	} else if err != nil {
		return 0, err
	}
	if len(ents) == 0 {
		return 0, nil
	}
	return ents[0].Term, nil
}

// LastIndex implements the raft.Storage interface.
// LastIndex requires that the replica lock is held.
func (r *Replica) LastIndex() (uint64, error) {
	return r.mu.lastIndex, nil
}

// raftTruncatedStateLocked returns metadata about the log that preceded the
// first current entry. This includes both entries that have been compacted away
// and the dummy entries that make up the starting point of an empty log.
// raftTruncatedStateLocked requires that the replica lock be held.
func (r *Replica) raftTruncatedStateLocked() (roachpb.RaftTruncatedState, error) {
	if r.mu.state.TruncatedState != nil {
		return *r.mu.state.TruncatedState, nil
	}
	ts, err := raftTruncatedState(r.store.Engine(), r.RangeID)
	if err != nil {
		return ts, err
	}
	if ts.Index != 0 {
		r.mu.state.TruncatedState = &ts
	}
	return ts, nil
}

func raftTruncatedState(
	eng engine.Reader, rangeID roachpb.RangeID,
) (roachpb.RaftTruncatedState, error) {
	ts := roachpb.RaftTruncatedState{}
	_, err := engine.MVCCGetProto(context.Background(), eng, keys.RaftTruncatedStateKey(rangeID),
		hlc.ZeroTimestamp, true, nil, &ts)
	return ts /* zero if not found */, err
}

// FirstIndex implements the raft.Storage interface.
// FirstIndex requires that the replica lock is held.
func (r *Replica) FirstIndex() (uint64, error) {
	ts, err := r.raftTruncatedStateLocked()
	if err != nil {
		return 0, err
	}
	return ts.Index + 1, nil
}

// GetFirstIndex is the same function as FirstIndex but it does not require
// that the replica lock is held.
func (r *Replica) GetFirstIndex() (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.FirstIndex()
}

// Snapshot implements the raft.Storage interface.
// Snapshot requires that the replica lock is held.
func (r *Replica) Snapshot() (raftpb.Snapshot, error) {
	rangeID := r.RangeID

	// If a snapshot is in progress, see if it's ready.
	if r.mu.snapshotChan != nil {
		select {
		case snapData, ok := <-r.mu.snapshotChan:
			if ok {
				return snapData, nil
			}
			// If the old channel was closed, fall through to start a new task.

		default:
			// If the result is not ready, return immediately.
			return raftpb.Snapshot{}, raft.ErrSnapshotTemporarilyUnavailable
		}
	}

	// See if there is already a snapshot running for this store.
	if !r.store.AcquireRaftSnapshot() {
		return raftpb.Snapshot{}, raft.ErrSnapshotTemporarilyUnavailable
	}

	// Use an unbuffered channel so the worker stays alive until someone
	// reads from the channel, and can abandon the snapshot if it gets stale.
	ch := make(chan (raftpb.Snapshot))

	if r.store.Stopper().RunAsyncTask(func() {
		defer close(ch)
		snap := r.store.NewSnapshot()
		defer snap.Close()
		defer r.store.ReleaseRaftSnapshot()
		// Delegate to a static function to make sure that we do not depend
		// on any indirect calls to r.store.Engine() (or other in-memory
		// state of the Replica). Everything must come from the snapshot.
		snapData, err := snapshot(snap, rangeID, r.mu.state.Desc.StartKey)
		if err != nil {
			log.Errorf("range %s: error generating snapshot: %s", rangeID, err)
		} else {
			select {
			case ch <- snapData:
			case <-time.After(r.store.ctx.AsyncSnapshotMaxAge):
				// If raft decides it doesn't need this snapshot any more (or
				// just takes too long to use it), abandon it to save memory.
			}
		}
	}) == nil {
		r.mu.snapshotChan = ch
	} else {
		r.store.ReleaseRaftSnapshot()
	}

	if r.store.ctx.BlockingSnapshotDuration > 0 {
		select {
		case snap, ok := <-r.mu.snapshotChan:
			if ok {
				return snap, nil
			}
		case <-time.After(r.store.ctx.BlockingSnapshotDuration):
		}
	}
	return raftpb.Snapshot{}, raft.ErrSnapshotTemporarilyUnavailable
}

// GetSnapshot wraps Snapshot() but does not require the replica lock
// to be held and it will block instead of returning
// ErrSnapshotTemporaryUnavailable.
func (r *Replica) GetSnapshot() (raftpb.Snapshot, error) {
	retryOptions := retry.Options{
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
		Multiplier:     2,
	}
	for retry := retry.Start(retryOptions); retry.Next(); {
		r.mu.Lock()
		snap, err := r.Snapshot()
		snapshotChan := r.mu.snapshotChan
		r.mu.Unlock()
		if err == raft.ErrSnapshotTemporarilyUnavailable {
			if snapshotChan == nil {
				// The call to Snapshot() didn't start an async process due to
				// rate limiting. Try again later.
				continue
			}
			var ok bool
			snap, ok = <-snapshotChan
			if ok {
				return snap, nil
			}
			// Each snapshot worker's output can only be consumed once.
			// We could be racing with raft itself, so if we get a closed
			// channel loop back and try again.
		} else {
			return snap, err
		}
	}
	panic("unreachable") // due to infinite retries
}

func snapshot(
	snap engine.Reader,
	rangeID roachpb.RangeID,
	startKey roachpb.RKey,
) (raftpb.Snapshot, error) {
	start := timeutil.Now()
	var snapData roachpb.RaftSnapshotData

	truncState, err := raftTruncatedState(snap, rangeID)
	if err != nil {
		return raftpb.Snapshot{}, err
	}
	firstIndex := truncState.Index + 1

	// Read the range metadata from the snapshot instead of the members
	// of the Range struct because they might be changed concurrently.
	appliedIndex, _, err := loadAppliedIndex(snap, rangeID)
	if err != nil {
		return raftpb.Snapshot{}, err
	}

	var desc roachpb.RangeDescriptor
	// We ignore intents on the range descriptor (consistent=false) because we
	// know they cannot be committed yet; operations that modify range
	// descriptors resolve their own intents when they commit.
	ok, err := engine.MVCCGetProto(context.Background(), snap, keys.RangeDescriptorKey(startKey),
		hlc.MaxTimestamp, false /* !consistent */, nil, &desc)
	if err != nil {
		return raftpb.Snapshot{}, errors.Errorf("failed to get desc: %s", err)
	}
	if !ok {
		return raftpb.Snapshot{}, errors.Errorf("couldn't find range descriptor")
	}

	// Store RangeDescriptor as metadata, it will be retrieved by ApplySnapshot()
	snapData.RangeDescriptor = desc

	// Iterate over all the data in the range, including local-only data like
	// the sequence cache.
	iter := NewReplicaDataIterator(&desc, snap, true /* replicatedOnly */)
	defer iter.Close()
	var alloc bufalloc.ByteAllocator
	for ; iter.Valid(); iter.Next() {
		var key engine.MVCCKey
		var value []byte
		alloc, key, value = iter.allocIterKeyValue(alloc)
		snapData.KV = append(snapData.KV,
			roachpb.RaftSnapshotData_KeyValue{
				Key:       key.Key,
				Value:     value,
				Timestamp: key.Timestamp,
			})
	}

	endIndex := appliedIndex + 1
	snapData.LogEntries = make([][]byte, 0, endIndex-firstIndex)

	scanFunc := func(kv roachpb.KeyValue) (bool, error) {
		bytes, err := kv.Value.GetBytes()
		if err == nil {
			snapData.LogEntries = append(snapData.LogEntries, bytes)
		}
		return false, err
	}

	if err := iterateEntries(snap, rangeID, firstIndex, endIndex, scanFunc); err != nil {
		return raftpb.Snapshot{}, err
	}

	data, err := protoutil.Marshal(&snapData)
	if err != nil {
		return raftpb.Snapshot{}, err
	}

	// Synthesize our raftpb.ConfState from desc.
	var cs raftpb.ConfState
	for _, rep := range desc.Replicas {
		cs.Nodes = append(cs.Nodes, uint64(rep.ReplicaID))
	}

	term, err := term(snap, rangeID, appliedIndex)
	if err != nil {
		return raftpb.Snapshot{}, errors.Errorf("failed to fetch term of %d: %s", appliedIndex, err)
	}

	log.Infof("generated snapshot for range %s at index %d in %s. encoded size=%d, %d KV pairs, %d log entries",
		rangeID, appliedIndex, timeutil.Since(start), len(data), len(snapData.KV), len(snapData.LogEntries))

	return raftpb.Snapshot{
		Data: data,
		Metadata: raftpb.SnapshotMetadata{
			Index:     appliedIndex,
			Term:      term,
			ConfState: cs,
		},
	}, nil
}

// append the given entries to the raft log. Takes the previous value
// of r.lastIndex and returns a new value. We do this rather than
// modifying r.lastIndex directly because this modification needs to
// be atomic with the commit of the batch.
func (r *Replica) append(batch engine.ReadWriter, prevLastIndex uint64, entries []raftpb.Entry) (uint64, error) {
	if len(entries) == 0 {
		return prevLastIndex, nil
	}
	for i := range entries {
		ent := &entries[i]
		key := keys.RaftLogKey(r.RangeID, ent.Index)
		if err := engine.MVCCPutProto(context.Background(), batch, nil, key, hlc.ZeroTimestamp, nil, ent); err != nil {
			return 0, err
		}
	}
	lastIndex := entries[len(entries)-1].Index
	// Delete any previously appended log entries which never committed.
	for i := lastIndex + 1; i <= prevLastIndex; i++ {
		err := engine.MVCCDelete(context.Background(), batch, nil,
			keys.RaftLogKey(r.RangeID, i), hlc.ZeroTimestamp, nil)
		if err != nil {
			return 0, err
		}
	}

	if err := setLastIndex(batch, r.RangeID, lastIndex); err != nil {
		return 0, err
	}

	return lastIndex, nil
}

// updateRangeInfo is called whenever a range is updated by ApplySnapshot
// or is created by range splitting to setup the fields which are
// uninitialized or need updating.
func (r *Replica) updateRangeInfo(desc *roachpb.RangeDescriptor) error {
	// RangeMaxBytes should be updated by looking up Zone Config in two cases:
	// 1. After applying a snapshot, if the zone config was not updated for
	// this key range, then maxBytes of this range will not be updated either.
	// 2. After a new range is created by a split, only copying maxBytes from
	// the original range wont work as the original and new ranges might belong
	// to different zones.
	// Load the system config.
	cfg, ok := r.store.Gossip().GetSystemConfig()
	if !ok {
		// This could be before the system config was ever gossiped,
		// or it expired. Let the gossip callback set the info.
		log.Warningf("no system config available, cannot determine range MaxBytes")
		return nil
	}

	// Find zone config for this range.
	zone, err := cfg.GetZoneConfigForKey(desc.StartKey)
	if err != nil {
		return errors.Errorf("failed to lookup zone config for Range %s: %s", r, err)
	}

	r.SetMaxBytes(zone.RangeMaxBytes)
	return nil
}

// applySnapshot updates the replica based on the given snapshot.
// Returns the new last index.
func (r *Replica) applySnapshot(snap raftpb.Snapshot) (uint64, error) {
	// We use a separate batch to apply the snapshot since the Replica (and in
	// particular the last index) is updated after the batch commits. Using a
	// separate batch also allows for future optimization (such as using a
	// Distinct() batch).
	batch := r.store.Engine().NewBatch()
	defer batch.Close()

	snapData := roachpb.RaftSnapshotData{}
	err := proto.Unmarshal(snap.Data, &snapData)
	if err != nil {
		return 0, err
	}

	// Extract the updated range descriptor.
	desc := snapData.RangeDescriptor

	r.mu.Lock()
	replicaID := r.mu.replicaID
	r.mu.Unlock()

	log.Infof("replica %d received snapshot for range %d at index %d. encoded size=%d, %d KV pairs, %d log entries",
		replicaID, desc.RangeID, snap.Metadata.Index, len(snap.Data), len(snapData.KV), len(snapData.LogEntries))
	defer func(start time.Time) {
		log.Infof("replica %d applied snapshot for range %d in %s", replicaID, desc.RangeID, timeutil.Since(start))
	}(timeutil.Now())

	// Delete everything in the range and recreate it from the snapshot.
	// We need to delete any old Raft log entries here because any log entries
	// that predate the snapshot will be orphaned and never truncated or GC'd.
	iter := NewReplicaDataIterator(&desc, batch, false /* !replicatedOnly */)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		if err := batch.Clear(iter.Key()); err != nil {
			return 0, err
		}
	}

	// Determine the unreplicated key prefix so we can drop any
	// unreplicated keys from the snapshot.
	unreplicatedPrefix := keys.MakeRangeIDUnreplicatedPrefix(desc.RangeID)

	// Write the snapshot into the range.
	for _, kv := range snapData.KV {
		if bytes.HasPrefix(kv.Key, unreplicatedPrefix) {
			continue
		}
		mvccKey := engine.MVCCKey{
			Key:       kv.Key,
			Timestamp: kv.Timestamp,
		}
		if err := batch.Put(mvccKey, kv.Value); err != nil {
			return 0, err
		}
	}

	logEntries := make([]raftpb.Entry, len(snapData.LogEntries))
	for i, bytes := range snapData.LogEntries {
		if err := logEntries[i].Unmarshal(bytes); err != nil {
			return 0, err
		}
	}

	// Write the snapshot's Raft log into the range.
	if _, err := r.append(batch, 0, logEntries); err != nil {
		return 0, err
	}

	s, err := loadState(batch, &desc)
	if err != nil {
		return 0, err
	}

	// As outlined above, last and applied index are the same after applying
	// the snapshot.
	if s.RaftAppliedIndex != snap.Metadata.Index {
		log.Fatalf("%d: snapshot resulted in appliedIndex=%d, metadataIndex=%d",
			s.Desc.RangeID, s.RaftAppliedIndex, snap.Metadata.Index)
	}

	if err := batch.Commit(); err != nil {
		return 0, err
	}

	r.mu.Lock()
	// We set the persisted last index to the last applied index. This is
	// not a correctness issue, but means that we may have just transferred
	// some entries we're about to re-request from the leader and overwrite.
	// However, raft.MultiNode currently expects this behaviour, and the
	// performance implications are not likely to be drastic. If our
	// feelings about this ever change, we can add a LastIndex field to
	// raftpb.SnapshotMetadata.
	r.mu.lastIndex = s.RaftAppliedIndex
	// Update the range and store stats.
	r.store.metrics.subtractMVCCStats(r.mu.state.Stats)
	r.store.metrics.addMVCCStats(s.Stats)
	r.mu.state = s
	lastIndex := r.mu.lastIndex
	r.assertStateLocked(r.store.Engine())
	r.mu.Unlock()

	// Update other fields which are uninitialized or need updating.
	// This may not happen if the system config has not yet been loaded.
	// While config update will correctly set the fields, there is no order
	// guarantee in ApplySnapshot.
	// TODO: should go through the standard store lock when adding a replica.
	if err := r.updateRangeInfo(&desc); err != nil {
		panic(err)
	}

	// Fill the reservation if there was any one for this range.
	r.store.bookie.Fill(desc.RangeID)

	// Update the range descriptor. This is done last as this is the step that
	// makes the Replica visible in the Store.
	if err := r.setDesc(&desc); err != nil {
		panic(err)
	}
	return lastIndex, nil
}

// Raft commands are encoded with a 1-byte version (currently 0), an 8-byte ID,
// followed by the payload. This inflexible encoding is used so we can efficiently
// parse the command id while processing the logs.
// TODO(bdarnell): Is this commandID still appropriate for our needs?
const (
	// The prescribed length for each command ID.
	raftCommandIDLen                = 8
	raftCommandEncodingVersion byte = 0
)

func encodeRaftCommand(commandID string, command []byte) []byte {
	if len(commandID) != raftCommandIDLen {
		log.Fatalf("invalid command ID length; %d != %d", len(commandID), raftCommandIDLen)
	}
	x := make([]byte, 1, 1+raftCommandIDLen+len(command))
	x[0] = raftCommandEncodingVersion
	x = append(x, []byte(commandID)...)
	x = append(x, command...)
	return x
}

// DecodeRaftCommand splits a raftpb.Entry.Data into its commandID and
// command portions. The caller is responsible for checking that the data
// is not empty (which indicates a dummy entry generated by raft rather
// than a real command). Usage is mostly internal to the storage package
// but is exported for use by debugging tools.
func DecodeRaftCommand(data []byte) (commandID string, command []byte) {
	if data[0] != raftCommandEncodingVersion {
		log.Fatalf("unknown command encoding version %v", data[0])
	}
	return string(data[1 : 1+raftCommandIDLen]), data[1+raftCommandIDLen:]
}
