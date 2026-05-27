package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a minimal JSON-RPC 1.0 client for the Bitmark node.
type Client struct {
	url  string
	user string
	pass string
	http *http.Client
}

func New(url, user, pass string) *Client {
	return &Client{url: url, user: user, pass: pass, http: &http.Client{}}
}

// NewWithTimeout creates a Client with an explicit HTTP timeout.
func NewWithTimeout(url, user, pass string, timeout time.Duration) *Client {
	return &Client{url: url, user: user, pass: pass, http: &http.Client{Timeout: timeout}}
}

// BlockExtra holds the proof-of-work and parent-block fields returned by getblock.
// These fields are not stored in the DB and are fetched live from the node.
type BlockExtra struct {
	PowHash              string `json:"powhash"`
	ParentBlockHash      string `json:"parentblockhash"`
	ParentBlockPowHash   string `json:"parentblockpowhash"`
	ParentBlockPrevHash  string `json:"parentblockprevhash"`
}

// GetBlockExtra calls getblock(hash, 2) and returns only the PoW/parent fields.
// Returns a zero-value BlockExtra (empty strings) on any error so callers can
// still render the page without these fields if the node is unavailable.
func (c *Client) GetBlockExtra(hash string) BlockExtra {
	var result BlockExtra
	_ = c.call("getblock", []any{hash, 2}, &result)
	return result
}

// GetBlockCount returns the node's current best block height.
func (c *Client) GetBlockCount() (int, error) {
	var n int
	err := c.call("getblockcount", []any{}, &n)
	return n, err
}

type rpcRequest struct {
	Method string `json:"method"`
	Params []any  `json:"params"`
	ID     int    `json:"id"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  any             `json:"error"`
}

func (c *Client) call(method string, params []any, out any) error {
	body, _ := json.Marshal(rpcRequest{Method: method, Params: params, ID: 1})
	req, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "close")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var rr rpcResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return fmt.Errorf("rpc unmarshal: %w (body=%s)", err, raw)
	}
	if rr.Error != nil {
		return fmt.Errorf("rpc error: %v", rr.Error)
	}
	return json.Unmarshal(rr.Result, out)
}

// AlgoNames maps algo_id → canonical name used in chaindynamics keys.
var AlgoNames = []string{
	"SCRYPT", "SHA256D", "YESCRYPT", "ARGON2", "X17", "LYRA2REv2", "EQUIHASH", "CRYPTONIGHT",
}

// ChainDynamicsResult holds the values we parse out of the chaindynamics response.
type ChainDynamicsResult struct {
	// Indexed by algo_id (0–7)
	Difficulty   [8]float64
	Hashrate     [8]float64
	PeakHashrate [8]float64
	BlockSpacing [8]float64 // minutes
	NSSFBlocks   [8]int     // nblocks until next SSF update
}

// ChainDynamics calls the chaindynamics RPC and returns parsed per-algo stats.
func (c *Client) ChainDynamics() (ChainDynamicsResult, error) {
	var raw map[string]any
	if err := c.call("chaindynamics", nil, &raw); err != nil {
		return ChainDynamicsResult{}, err
	}

	var res ChainDynamicsResult
	for i, name := range AlgoNames {
		if v, ok := raw["difficulty "+name]; ok {
			res.Difficulty[i] = toFloat(v)
		}
		if v, ok := raw["current hashrate "+name]; ok {
			res.Hashrate[i] = toFloat(v)
		}
		if v, ok := raw["peak hashrate "+name]; ok {
			res.PeakHashrate[i] = toFloat(v)
		}
		if v, ok := raw["average block spacing "+name]; ok {
			// value is in seconds; convert to minutes
			res.BlockSpacing[i] = toFloat(v) / 60.0
		}
		if v, ok := raw["nblocks update SSF "+name]; ok {
			res.NSSFBlocks[i] = int(toFloat(v))
		}
	}
	return res, nil
}

// MoneySupply calls getmoneysupply(algoID) and returns the value in BTM.
// The RPC returns {"money supply": <float>}.
func (c *Client) MoneySupply(algoID int) (float64, error) {
	var result map[string]float64
	if err := c.call("getmoneysupply", []any{algoID}, &result); err != nil {
		return 0, err
	}
	return result["money supply"], nil
}

// BlockReward calls getblockreward(algoID) and returns the SSF-scaled current reward in BTM.
// The RPC returns {"block reward": <float>}.
func (c *Client) BlockReward(algoID int) (float64, error) {
	var result map[string]float64
	if err := c.call("getblockreward", []any{algoID}, &result); err != nil {
		return 0, err
	}
	return result["block reward"], nil
}

// NominalBlockReward calls getblockreward(algoID, -1, true) and returns the
// unscaled epoch nominal reward in BTM (i.e. without SSF adjustment).
func (c *Client) NominalBlockReward(algoID int) (float64, error) {
	var result map[string]float64
	if err := c.call("getblockreward", []any{algoID, -1, true}, &result); err != nil {
		return 0, err
	}
	return result["block reward"], nil
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
