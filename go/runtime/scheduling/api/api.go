// Package api implements the transaction scheduler algorithm API.
package api

import (
	"github.com/oasisprotocol/oasis-core/go/common/crypto/hash"
	"github.com/oasisprotocol/oasis-core/go/runtime/transaction"
)

// Scheduler defines an algorithm for scheduling incoming transactions.
type Scheduler interface {
	// Name is the scheduler algorithm name.
	Name() string

	// QueueTx queues a transaction for scheduling.
	QueueTx(tx *transaction.CheckedTransaction) error

	// RemoveTxBatch removes a transaction batch.
	RemoveTxBatch(tx []hash.Hash)

	// GetBatch returns a batch of scheduled transactions (if any is available).
	GetBatch(force bool) []*transaction.CheckedTransaction

	// GetPrioritizedBatch returns a batch of transactions ordered by priority but without taking
	// any weight limits into account.
	//
	// Offset specifies the transaction hash that should serve as an offset when returning
	// transactions from the pool. Transactions will be skipped until the given hash is encountered
	// and only following transactions will be returned.
	GetPrioritizedBatch(offset *hash.Hash, limit uint32) []*transaction.CheckedTransaction

	// GetKnownBatch gets a set of known transactions from the transaction pool.
	//
	// For any missing transactions nil will be returned in their place and the map of missing
	// transactions will be populated accoordingly.
	GetKnownBatch(batch []hash.Hash) ([]*transaction.CheckedTransaction, map[hash.Hash]int)

	// GetTransactions returns the given number of transactions from the transaction pool without
	// taking any batch limits or priorities into account.
	//
	// Specifying a zero limit will return all transactions.
	GetTransactions(limit int) []*transaction.CheckedTransaction

	// UnscheduledSize returns number of unscheduled items.
	UnscheduledSize() uint64

	// IsQueued returns if a transaction is queued.
	IsQueued(hash.Hash) bool

	// UpdateParameters updates the scheduling parameters.
	UpdateParameters(weightLimits map[transaction.Weight]uint64)

	// Clear clears the transaction queue.
	Clear()
}
