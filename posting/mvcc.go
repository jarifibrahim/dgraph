/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package posting

import (
	"bytes"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"go.opencensus.io/stats"

	"github.com/dgraph-io/badger"
	"github.com/dgraph-io/dgo/protos/api"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/x"
	farm "github.com/dgryski/go-farm"
	"github.com/golang/glog"
)

var (
	ErrTsTooOld = x.Errorf("Transaction is too old")
)

func (t *Txn) SetAbort() {
	atomic.StoreUint32(&t.shouldAbort, 1)
}

func (t *Txn) ShouldAbort() bool {
	if t == nil {
		return false
	}
	return atomic.LoadUint32(&t.shouldAbort) > 0
}

func (t *Txn) AddKeys(key, conflictKey string) {
	t.Lock()
	defer t.Unlock()
	if t.deltas == nil || t.conflicts == nil {
		t.deltas = make(map[string]struct{})
		t.conflicts = make(map[string]struct{})
	}
	t.deltas[key] = struct{}{}
	if len(conflictKey) > 0 {
		t.conflicts[conflictKey] = struct{}{}
	}
}

func (t *Txn) Fill(ctx *api.TxnContext) {
	t.Lock()
	defer t.Unlock()
	ctx.StartTs = t.StartTs
	for key := range t.conflicts {
		// We don't need to send the whole conflict key to Zero. Solving #2338
		// should be done by sending a list of mutating predicates to Zero,
		// along with the keys to be used for conflict detection.
		fps := strconv.FormatUint(farm.Fingerprint64([]byte(key)), 36)
		if !x.HasString(ctx.Keys, fps) {
			ctx.Keys = append(ctx.Keys, fps)
		}
	}
	for key := range t.deltas {
		pk := x.Parse([]byte(key))
		if !x.HasString(ctx.Preds, pk.Attr) {
			ctx.Preds = append(ctx.Preds, pk.Attr)
		}
	}
}

// Don't call this for schema mutations. Directly commit them.
// This function only stores deltas to the commit timestamps. It does not try to generate a state.
// TODO: Simplify this function. All it should be doing is to store the deltas, and not try to
// generate state. The state should only be generated by rollup, which in turn should look at the
// last Snapshot Ts, to determine how much of the PL to rollup. We only want to roll up the deltas,
// with commit ts <= snapshot ts, and not above.
func (tx *Txn) CommitToDisk(writer *x.TxnWriter, commitTs uint64) error {
	if commitTs == 0 {
		return nil
	}
	var keys []string
	tx.Lock()
	for key := range tx.deltas {
		keys = append(keys, key)
	}
	tx.Unlock()

	// TODO: Simplify this. All we need to do is to get the PL for the key, and if it has the
	// postings for the startTs, we commit them. Otherwise, we skip.
	// Also, if the snapshot read ts is above the commit ts, then we just delete the postings from
	// memory, instead of writing them back again.

	for _, key := range keys {
		plist, err := tx.Get([]byte(key))
		if err != nil {
			return err
		}
		data := plist.GetMutation(tx.StartTs)
		if data == nil {
			continue
		}
		if err := writer.SetAt([]byte(key), data, BitDeltaPosting, commitTs); err != nil {
			return err
		}
	}
	return nil
}

func (tx *Txn) CommitToMemory(commitTs uint64) error {
	tx.Lock()
	defer tx.Unlock()
	// TODO: Figure out what shouldAbort is for, and use it correctly. This should really be
	// shouldDiscard.
	// defer func() {
	// 	atomic.StoreUint32(&tx.shouldAbort, 1)
	// }()
	for key := range tx.deltas {
	inner:
		for {
			plist, err := tx.Get([]byte(key))
			if err != nil {
				return err
			}
			err = plist.CommitMutation(tx.StartTs, commitTs)
			switch err {
			case nil:
				break inner
			case ErrRetry:
				time.Sleep(5 * time.Millisecond)
			default:
				glog.Warningf("Error while committing to memory: %v\n", err)
				return err
			}
		}
	}
	return nil
}

func unmarshalOrCopy(plist *pb.PostingList, item *badger.Item) error {
	// It's delta
	return item.Value(func(val []byte) error {
		if len(val) == 0 {
			// empty pl
			return nil
		}
		return plist.Unmarshal(val)
	})
}

// constructs the posting list from the disk using the passed iterator.
// Use forward iterator with allversions enabled in iter options.
//
// key would now be owned by the posting list. So, ensure that it isn't reused
// elsewhere.
func ReadPostingList(key []byte, it *badger.Iterator) (*List, error) {
	l := new(List)
	l.key = key
	l.mutationMap = make(map[uint64]*pb.PostingList)
	l.plist = new(pb.PostingList)

	// Iterates from highest Ts to lowest Ts
	for it.Valid() {
		item := it.Item()
		if item.IsDeletedOrExpired() {
			// Don't consider any more versions.
			break
		}
		if !bytes.Equal(item.Key(), l.key) {
			break
		}

		if item.UserMeta()&BitCompletePosting > 0 {
			if err := unmarshalOrCopy(l.plist, item); err != nil {
				return nil, err
			}
			l.minTs = item.Version()
			// No need to do Next here. The outer loop can take care of skipping more versions of
			// the same key.
			break
		}

		if item.UserMeta()&BitDeltaPosting > 0 {
			err := item.Value(func(val []byte) error {
				pl := &pb.PostingList{}
				x.Check(pl.Unmarshal(val))
				pl.CommitTs = item.Version()
				for _, mpost := range pl.Postings {
					// commitTs, startTs are meant to be only in memory, not
					// stored on disk.
					mpost.CommitTs = item.Version()
				}
				l.mutationMap[pl.CommitTs] = pl
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else {
			x.Fatalf("unexpected meta: %d", item.UserMeta())
		}
		if item.DiscardEarlierVersions() {
			break
		}
		it.Next()
	}
	return l, nil
}

func getNew(key []byte, pstore *badger.DB) (*List, error) {
	l := new(List)
	l.key = key
	l.mutationMap = make(map[uint64]*pb.PostingList)
	l.plist = new(pb.PostingList)
	txn := pstore.NewTransactionAt(math.MaxUint64, false)
	defer txn.Discard()

	item, err := txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if item.UserMeta()&BitCompletePosting > 0 {
		err = unmarshalOrCopy(l.plist, item)
		l.minTs = item.Version()
	} else {
		iterOpts := badger.DefaultIteratorOptions
		iterOpts.AllVersions = true
		it := txn.NewKeyIterator(key, iterOpts)
		defer it.Close()
		it.Seek(key)
		l, err = ReadPostingList(key, it)
	}
	if err != nil {
		return l, err
	}

	l.Lock()
	size := l.calculateSize()
	l.Unlock()

	// Record the size
	stats.Record(x.ObservabilityEnabledParentContext(), x.BytesRead.M(int64(size)))

	atomic.StoreInt32(&l.estimatedSize, size)
	return l, nil
}
