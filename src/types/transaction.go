package types

import (
	"time"
)

// Transaction represents a Bitcoin transaction
type Transaction struct {
	TxID         Hash32     `json:"txid"`
	FirstSeen    time.Time  `json:"firstSeen"`
	LastRemoved  *time.Time `json:"lastRemoved"`
	Fee          uint64     `json:"fee"`
	Weight       int        `json:"weight"`
	BlockHeight  int32      `json:"blockHeight"`
	IndexInBlock int32      `json:"indexInBlock"`
}

// StoredTransaction extends Transaction with  Database ID
type StoredTransaction struct {
	// Internal database ID
	DBID int64
	Transaction
}
