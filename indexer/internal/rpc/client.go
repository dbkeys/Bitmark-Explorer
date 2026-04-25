package rpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bitmark/bitmark-indexer/internal/config"
)

type Client struct {
	url           string
	authHeader    string
	http          *http.Client
	maxResponseSz int64
}

func NewClient(cfg config.RPCConfig) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if strings.HasPrefix(strings.ToLower(cfg.URL), "https://") && cfg.InsecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	basic := base64.StdEncoding.EncodeToString([]byte(cfg.User + ":" + cfg.Pass))
	return &Client{
		url:           cfg.URL,
		authHeader:    "Basic " + basic,
		http:          &http.Client{Transport: transport, Timeout: cfg.Timeout},
		maxResponseSz: cfg.MaxResponseSize,
	}
}

type rpcReq struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      string      `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	ID     string          `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

func (c *Client) call(ctx context.Context, method string, params []any, out any) error {
	body, _ := json.Marshal(rpcReq{
		JSONRPC: "1.0",
		ID:      "bitmark-indexer",
		Method:  method,
		Params:  params,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseSz))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("rpc http %d: %s", resp.StatusCode, string(raw))
	}

	var rr rpcResp
	if err := json.Unmarshal(raw, &rr); err != nil {
		return fmt.Errorf("rpc decode: %w", err)
	}
	if rr.Error != nil {
		return rr.Error
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rr.Result, out)
}

func (c *Client) GetBlockCount(ctx context.Context) (int, error) {
	var n int
	err := c.call(ctx, "getblockcount", []any{}, &n)
	return n, err
}

func (c *Client) GetBlockHash(ctx context.Context, height int) (string, error) {
	var s string
	err := c.call(ctx, "getblockhash", []any{height}, &s)
	return s, err
}

func (c *Client) GetBlockVerbose2(ctx context.Context, hash string) (*BlockV2, error) {
	var b BlockV2
	err := c.call(ctx, "getblock", []any{hash, 2}, &b)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// AlgoNames maps algo_id (0-7) to the canonical name used in chaindynamics keys.
var AlgoNames = [8]string{
	"SCRYPT", "SHA256D", "YESCRYPT", "ARGON2", "X17", "LYRA2REv2", "EQUIHASH", "CRYPTONIGHT",
}

// ChainDynamicsResult holds per-algo values parsed from the chaindynamics RPC.
type ChainDynamicsResult struct {
	Difficulty   [8]float64
	Hashrate     [8]float64
	PeakHashrate [8]float64
	NSSFBlocks   [8]int
	BlockSpacing [8]float64 // average minutes between consecutive same-algo blocks
}

// ChainDynamics calls the chaindynamics RPC and returns parsed per-algo stats.
func (c *Client) ChainDynamics(ctx context.Context) (ChainDynamicsResult, error) {
	var raw map[string]any
	if err := c.call(ctx, "chaindynamics", nil, &raw); err != nil {
		return ChainDynamicsResult{}, err
	}
	var res ChainDynamicsResult
	for i, name := range AlgoNames {
		if v, ok := raw["difficulty "+name]; ok {
			res.Difficulty[i] = anyToFloat(v)
		}
		if v, ok := raw["current hashrate "+name]; ok {
			res.Hashrate[i] = anyToFloat(v)
		}
		if v, ok := raw["peak hashrate "+name]; ok {
			res.PeakHashrate[i] = anyToFloat(v)
		}
		if v, ok := raw["nblocks update SSF "+name]; ok {
			res.NSSFBlocks[i] = int(anyToFloat(v))
		}
		// chaindynamics returns spacing already in minutes — use directly.
		if v, ok := raw["average block spacing "+name]; ok {
			res.BlockSpacing[i] = anyToFloat(v)
		}
	}
	return res, nil
}

// MoneySupply calls getmoneysupply(algoID) → {"money supply": <BTM float>}.
func (c *Client) MoneySupply(ctx context.Context, algoID int) (float64, error) {
	var result map[string]float64
	if err := c.call(ctx, "getmoneysupply", []any{algoID}, &result); err != nil {
		return 0, err
	}
	return result["money supply"], nil
}

// BlockReward calls getblockreward(algoID) → {"block reward": <BTM float>}.
func (c *Client) BlockReward(ctx context.Context, algoID int) (float64, error) {
	var result map[string]float64
	if err := c.call(ctx, "getblockreward", []any{algoID}, &result); err != nil {
		return 0, err
	}
	return result["block reward"], nil
}

// NominalBlockReward calls getblockreward(algoID, -1, true) to get the
// unscaled epoch nominal reward in BTM (without SSF adjustment).
func (c *Client) NominalBlockReward(ctx context.Context, algoID int) (float64, error) {
	var result map[string]float64
	if err := c.call(ctx, "getblockreward", []any{algoID, -1, true}, &result); err != nil {
		return 0, err
	}
	return result["block reward"], nil
}

func anyToFloat(v any) float64 {
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
