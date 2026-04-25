package rpc

// Models for "getblock <hash> 2" responses.

type BlockV2 struct {
	Hash              string  `json:"hash"`
	Confirmations     int     `json:"confirmations"`
	Size              int     `json:"size"`
	Weight            int     `json:"weight"`
	Height            int     `json:"height"`
	Version           int     `json:"version"`
	MerkleRoot        string  `json:"merkleroot"`
	Time              int64   `json:"time"`
	Nonce             uint64  `json:"nonce"`
	Bits              string  `json:"bits"`
	Difficulty        float64 `json:"difficulty"`
	ChainWork         string  `json:"chainwork"`
	PreviousBlockHash string  `json:"previousblockhash"`

	// Bitmark/explorer extras (best-effort; may be absent)
	AlgoID      *int   `json:"algo_id,omitempty"`
	AlgoName    string `json:"algo,omitempty"`
	AuxPow      bool   `json:"auxpow,omitempty"`
	AuxPowSign  string `json:"auxpowsign,omitempty"`
	CoreVersion string `json:"coreversion,omitempty"`

	Tx []TxV2 `json:"tx"`
}

type TxV2 struct {
	Txid     string  `json:"txid"`
	Version  *int    `json:"version,omitempty"`
	Locktime *int64  `json:"locktime,omitempty"`
	Size     *int    `json:"size,omitempty"`
	Vsize    *int    `json:"vsize,omitempty"`
	Weight   *int    `json:"weight,omitempty"`
	Vin      []TxIn  `json:"vin"`
	Vout     []TxOut `json:"vout"`
}

type TxIn struct {
	Txid      string     `json:"txid,omitempty"`
	Vout      *int       `json:"vout,omitempty"`
	ScriptSig ScriptSig  `json:"scriptSig,omitempty"`
	Sequence  *int64     `json:"sequence,omitempty"`
	Coinbase  string     `json:"coinbase,omitempty"`
}

type ScriptSig struct {
	Asm string `json:"asm,omitempty"`
	Hex string `json:"hex,omitempty"`
}

type TxOut struct {
	Value        float64      `json:"value"`
	N            uint32       `json:"n"`
	ScriptPubKey ScriptPubKey `json:"scriptPubKey"`
}

type ScriptPubKey struct {
	Asm       string   `json:"asm,omitempty"`
	Hex       string   `json:"hex,omitempty"`
	Type      string   `json:"type,omitempty"`
	Address   string   `json:"address,omitempty"`   // modern single-address field
	Addresses []string `json:"addresses,omitempty"` // legacy multi-address field
}

// FirstAddress returns the address from either the modern or legacy field.
func (s ScriptPubKey) FirstAddress() string {
	if s.Address != "" {
		return s.Address
	}
	if len(s.Addresses) > 0 {
		return s.Addresses[0]
	}
	return ""
}

