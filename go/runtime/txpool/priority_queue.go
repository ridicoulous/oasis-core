package txpool

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/google/btree"

	"github.com/oasisprotocol/oasis-core/go/common/crypto/hash"
	"github.com/oasisprotocol/oasis-core/go/runtime/transaction"
	p2pError "github.com/oasisprotocol/oasis-core/go/worker/common/p2p/error"
)

type item struct {
	tx *transaction.CheckedTransaction
}

func (i item) Less(other btree.Item) bool {
	i2 := other.(*item)
	if p1, p2 := i.tx.Priority(), i2.tx.Priority(); p1 != p2 {
		return p1 < p2
	}
	// If transactions have same priority, sort arbitrary.
	h1 := i.tx.Hash()
	h2 := i2.tx.Hash()
	return bytes.Compare(h1[:], h2[:]) < 0
}

type priorityQueue struct {
	sync.Mutex

	priorityIndex *btree.BTree
	transactions  map[hash.Hash]*item

	maxTxPoolSize uint64

	poolWeights  map[transaction.Weight]uint64
	weightLimits map[transaction.Weight]uint64

	lowestPriority uint64
}

func (q *priorityQueue) Add(tx *transaction.CheckedTransaction) error {
	q.Lock()
	defer q.Unlock()

	// Check if there is room in the queue.
	var needsPop bool
	if q.poolWeights[transaction.WeightCount] >= q.maxTxPoolSize {
		needsPop = true

		if tx.Priority() <= q.lowestPriority {
			return fmt.Errorf("tx pool is full")
		}
	}

	if err := q.checkTxLocked(tx); err != nil {
		return err
	}

	// Remove the lowest priority transaction when queue is full.
	if needsPop {
		lpi := q.priorityIndex.Min()
		if lpi != nil {
			q.removeTxsLocked([]*item{lpi.(*item)})
		}
	}

	item := &item{tx: tx}
	q.priorityIndex.ReplaceOrInsert(item)
	q.transactions[tx.Hash()] = item
	for k, v := range tx.Weights() {
		q.poolWeights[k] += v
	}
	if tx.Priority() < q.lowestPriority {
		q.lowestPriority = tx.Priority()
	}

	if mlen, qlen := len(q.transactions), q.priorityIndex.Len(); mlen != qlen {
		panic(fmt.Errorf("inconsistent sizes of the underlying index (%v) and map (%v) after Add", mlen, qlen))
	}
	if mlen, plen := uint64(len(q.transactions)), q.poolWeights[transaction.WeightCount]; mlen != plen {
		panic(fmt.Errorf("inconsistent sizes of the map (%v) and pool weight count (%v) after Add", mlen, plen))
	}

	return nil
}

func (q *priorityQueue) GetBatch(force bool) []*transaction.CheckedTransaction {
	q.Lock()
	defer q.Unlock()

	// Check if a batch is ready.
	var weightLimitReached bool
	for k, v := range q.weightLimits {
		if q.poolWeights[k] >= v {
			weightLimitReached = true
			break
		}
	}
	if !weightLimitReached && !force {
		return nil
	}

	minWeights := map[transaction.Weight]uint64{
		transaction.WeightCount:             1,
		transaction.WeightSizeBytes:         10,
		transaction.WeightConsensusMessages: 0,
	}

	var batch []*transaction.CheckedTransaction
	batchWeights := make(map[transaction.Weight]uint64)
	for w := range q.weightLimits {
		batchWeights[w] = 0
	}
	toRemove := []*item{}
	q.priorityIndex.Descend(func(i btree.Item) bool {
		item := i.(*item)

		// Check if the call fits into the batch.
		for w, limit := range q.weightLimits {
			batchWeight := batchWeights[w]

			txW := item.tx.Weight(w)
			// Transaction weight greater than the limit. Drop the tx from the pool.
			if txW > limit {
				toRemove = append(toRemove, item)
				return true
			}

			// Stop if we can't actually fit anything in the batch.
			if limit-batchWeight < minWeights[w] {
				return false
			}

			// This transaction would overflow the batch.
			if batchWeight+txW > limit {
				return true
			}
		}

		// Add the tx to the batch.
		batch = append(batch, item.tx)
		for w, val := range item.tx.Weights() {
			if _, ok := batchWeights[w]; ok {
				batchWeights[w] += val
			}
		}

		return true
	})

	// Remove transactions discovered to be too big to even fit the batch.
	// This can happen if weight limits changed after the transaction was
	// already set to be scheduled.
	q.removeTxsLocked(toRemove)

	return batch
}

func (q *priorityQueue) removeTxsLocked(items []*item) {
	for _, item := range items {
		// Skip already removed items to avoid corrupting the list in case of duplicates.
		if _, exists := q.transactions[item.tx.Hash()]; !exists {
			continue
		}

		delete(q.transactions, item.tx.Hash())
		q.priorityIndex.Delete(item)
		for k, v := range item.tx.Weights() {
			q.poolWeights[k] -= v
		}
	}

	// Update lowest priority.
	if len(items) > 0 {
		if lpi := q.priorityIndex.Min(); lpi != nil {
			q.lowestPriority = lpi.(*item).tx.Priority()
		} else {
			q.lowestPriority = 0
		}
	}

	if mlen, qlen := len(q.transactions), q.priorityIndex.Len(); mlen != qlen {
		panic(fmt.Errorf("inconsistent sizes of the underlying index (%v) and map (%v) after removal", mlen, qlen))
	}
	if mlen, plen := uint64(len(q.transactions)), q.poolWeights[transaction.WeightCount]; mlen != plen {
		panic(fmt.Errorf("inconsistent sizes of the map (%v) and pool weight count (%v) after removal", mlen, plen))
	}
}

func (q *priorityQueue) GetPrioritizedBatch(offset *hash.Hash, limit uint32) []*transaction.CheckedTransaction {
	q.Lock()
	defer q.Unlock()

	var (
		batch      []*transaction.CheckedTransaction
		toRemove   []*item
		offsetItem btree.Item
	)
	if offset != nil {
		var exists bool
		offsetItem, exists = q.transactions[*offset]
		if !exists {
			// Offset does not exist so no items will be matched anyway.
			return nil
		}
	}
	q.priorityIndex.DescendLessOrEqual(offsetItem, func(i btree.Item) bool {
		item := i.(*item)

		for w, l := range q.weightLimits {
			txW := item.tx.Weight(w)
			// Transaction weight greater than the limit. Drop the tx from the pool.
			if txW > l {
				toRemove = append(toRemove, item)
				return true
			}
		}

		// Skip the offset item itself (if specified).
		if txHash := item.tx.Hash(); txHash.Equal(offset) {
			return true
		}

		// Add the tx to the batch.
		batch = append(batch, item.tx)
		if uint32(len(batch)) >= limit { //nolint: gosimple
			return false
		}
		return true
	})

	// Remove transactions discovered to be too big to even fit the batch.
	// This can happen if weight limits changed after the transaction was
	// already set to be scheduled.
	q.removeTxsLocked(toRemove)

	return batch
}

func (q *priorityQueue) GetKnownBatch(batch []hash.Hash) ([]*transaction.CheckedTransaction, map[hash.Hash]int) {
	q.Lock()
	defer q.Unlock()

	result := make([]*transaction.CheckedTransaction, 0, len(batch))
	missing := make(map[hash.Hash]int)
	for index, txHash := range batch {
		if item, ok := q.transactions[txHash]; ok {
			result = append(result, item.tx)
		} else {
			result = append(result, nil)
			missing[txHash] = index
		}
	}
	return result, missing
}

func (q *priorityQueue) GetTransactions(limit int) []*transaction.CheckedTransaction {
	q.Lock()
	defer q.Unlock()

	count := len(q.transactions)
	if limit > 0 && limit < count {
		count = limit
	}

	result := make([]*transaction.CheckedTransaction, 0, count)
	for _, item := range q.transactions {
		if len(result) >= count {
			break
		}
		result = append(result, item.tx)
	}
	return result
}

func (q *priorityQueue) RemoveTxBatch(batch []hash.Hash) {
	q.Lock()
	defer q.Unlock()

	items := make([]*item, 0, len(batch))
	for _, txHash := range batch {
		if item, ok := q.transactions[txHash]; ok {
			items = append(items, item)
		}
	}
	q.removeTxsLocked(items)
}

func (q *priorityQueue) IsQueued(txHash hash.Hash) bool {
	q.Lock()
	defer q.Unlock()

	return q.isQueuedLocked(txHash)
}

func (q *priorityQueue) Size() uint64 {
	q.Lock()
	defer q.Unlock()

	return q.poolWeights[transaction.WeightCount]
}

func (q *priorityQueue) UpdateMaxPoolSize(maxPoolSize uint64) {
	q.Lock()
	defer q.Unlock()

	q.maxTxPoolSize = maxPoolSize
	// Any transaction not within the new limits will get removed during GetBatch iteration.
}

func (q *priorityQueue) UpdateWeightLimits(limits map[transaction.Weight]uint64) {
	q.Lock()
	defer q.Unlock()

	q.weightLimits = limits
	// Any transaction not within the new limits will get removed during GetBatch iteration.
}

func (q *priorityQueue) Clear() {
	q.Lock()
	defer q.Unlock()

	q.priorityIndex.Clear(true)
	q.transactions = make(map[hash.Hash]*item)
	q.poolWeights = make(map[transaction.Weight]uint64)
	q.lowestPriority = 0
}

// NOTE: Assumes lock is held.
func (q *priorityQueue) checkTxLocked(tx *transaction.CheckedTransaction) error {
	// Check weights.
	for w, l := range q.weightLimits {
		txW := tx.Weight(w)
		if txW > l {
			return p2pError.Permanent(fmt.Errorf("call too large"))
		}
	}

	if q.isQueuedLocked(tx.Hash()) {
		return fmt.Errorf("tx already exists in pool")
	}

	return nil
}

// NOTE: Assumes lock is held.
func (q *priorityQueue) isQueuedLocked(txHash hash.Hash) bool {
	_, ok := q.transactions[txHash]
	return ok
}

func newPriorityQueue(maxPoolSize uint64, weightLimits map[transaction.Weight]uint64) *priorityQueue {
	return &priorityQueue{
		transactions:  make(map[hash.Hash]*item),
		poolWeights:   make(map[transaction.Weight]uint64),
		priorityIndex: btree.New(2),
		maxTxPoolSize: maxPoolSize,
		weightLimits:  weightLimits,
	}
}
