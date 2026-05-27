package db

import "time"


// Block represents a blockchain block (used in the latest-blocks table)
type Block struct {
	Height      int64
	Hash        string
	Difficulty  float64
	TimeUTC     time.Time
	TxCount     int
	TotalOut    int64
	TotalOutBTM float64
	AlgoID      int
	AlgoName    *string
	Version     int64
	CoreVersion *string
	Auxpow      bool
}

// AlgoStat represents statistics for a mining algorithm
type AlgoStat struct {
	AlgoID         int
	AlgoName       string
	Difficulty     float64
	Hashrate       float64
	PeakHashrate   float64
	MoneySupply    int64
	MoneySupplyBTM float64 // precomputed: MoneySupply / 1e8
	BlockReward        float64
	NominalBlockReward float64
	NSSFRemaining      int
	BlockSpacing   float64
	ComputedAt     time.Time
}

// GlobalStats holds chain-level aggregate statistics
type GlobalStats struct {
	NumBlocks    int64
	MoneySupply  float64 // BTM (sum across all algos)
	BlockSpacing float64 // average minutes between consecutive blocks
}

// BlockDetail holds every field needed for the block detail page.
type BlockDetail struct {
	Height        int64
	Hash          string
	PrevHash      *string
	NextHash      *string
	MerkleRoot    *string
	TimeUTC       time.Time
	TimeUnix      int64
	Version       int64
	CoreVersion   *string
	Bits          *string
	Nonce         int64
	SizeBytes     *int
	Weight        *int
	Difficulty    float64
	Chainwork     *string
	TxCount       int
	TotalOut      int64
	TotalOutBTM   float64
	AlgoID        *int
	AlgoName      *string
	Auxpow        bool
	AuxpowSign    *string
	Confirmations int64
	// Proof-of-work fields fetched live from node RPC (empty when node unavailable)
	PowHash             string
	ParentBlockHash     string
	ParentBlockPowHash  string
	ParentBlockPrevHash string
	Txs           []TxDetail
}

// TxDetail holds a decoded transaction.
type TxDetail struct {
	Txid        string
	IsCoinbase  bool
	TotalOut    int64
	TotalOutBTM float64
	Inputs      []TxInputDetail
	Outputs     []TxOutputDetail
}

// TxInputDetail is one input of a transaction.
type TxInputDetail struct {
	PrevTxid    *string
	PrevVout    *int
	PrevAddr    *string
	PrevVal     int64
	PrevValBTM  float64
	IsCoinbase  bool
	CoinbaseHex *string
}

// TxOutputDetail is one output of a transaction.
type TxOutputDetail struct {
	Vout       int
	Val        int64
	ValBTM     float64
	ScriptType *string
	Address    *string
}

// AddressDetail holds everything needed for the address detail page.
type AddressDetail struct {
	Address       string
	BalanceSats   int64
	BalanceBTM    float64
	TotalRecvSats int64
	TotalRecvBTM  float64
	TotalSentSats int64
	TotalSentBTM  float64
	TxCount       int
	Txs           []AddressTxRow
}

// AddressTxRow is one transaction in the address history.
type AddressTxRow struct {
	Txid        string
	BlockHeight int64
	TimeUTC     time.Time
	NetSats     int64   // positive = net received, negative = net spent
	NetBTM      float64
	BalanceSats int64   // running balance after this tx
	BalanceBTM  float64
}
