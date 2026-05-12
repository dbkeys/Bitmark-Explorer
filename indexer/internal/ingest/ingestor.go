package ingest

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/bitmark/bitmark-indexer/internal/config"
	"github.com/bitmark/bitmark-indexer/internal/rpc"
	"github.com/bitmark/bitmark-indexer/internal/zmq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rpcRetryBackoff holds the sequence of delays (seconds) used when retrying transient
// RPC errors.  The last value is repeated indefinitely, so the maximum back-off is
// 10 minutes — enough to let an overloaded node catch its breath without hammering it.
var rpcRetryBackoff = []time.Duration{5, 10, 20, 30, 60, 120, 300, 600, 600}

// isTransientRPCErr returns true for errors that are safe to retry:
// HTTP 503 (work queue full), HTTP 500 code -28 (node still loading),
// connection timeouts, and brief connection resets.
func isTransientRPCErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "503") ||
		strings.Contains(s, "Work queue depth exceeded") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "Loading block index") ||
		strings.Contains(s, `"code":-28`)
}

// retryGetBlockCount calls GetBlockCount, retrying with backoff on transient
// errors until the context is cancelled or a permanent error occurs.
func (i *Ingestor) retryGetBlockCount(ctx context.Context) (int, error) {
	for attempt := 0; ; attempt++ {
		n, err := i.rpc.GetBlockCount(ctx)
		if err == nil {
			return n, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if !isTransientRPCErr(err) {
			return 0, fmt.Errorf("rpc getblockcount: %w", err)
		}
		delay := rpcRetryBackoff[min(attempt, len(rpcRetryBackoff)-1)] * time.Second
		log.Printf("WARNING: rpc getblockcount transient error (attempt %d): %v — retrying in %s", attempt+1, err, delay)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type Ingestor struct {
	cfg               config.Config
	db                *pgxpool.Pool
	rpc               *rpc.Client
	lastHashrateCount [8]int64 // algo block count at the time of the last hashrate update
}

func New(cfg config.Config, db *pgxpool.Pool, rpcClient *rpc.Client) *Ingestor {
	return &Ingestor{cfg: cfg, db: db, rpc: rpcClient}
}

func (i *Ingestor) Run(ctx context.Context) error {
	if err := i.ensureChainState(ctx); err != nil {
		return err
	}

	// Connect ZMQ subscriber for real-time block notifications at tip.
	// If ZMQ is unavailable, we fall back to RPC polling gracefully.
	var sub *zmq.Subscriber
	if i.cfg.ZMQEndpoint != "" {
		var err error
		sub, err = zmq.NewSubscriber(i.cfg.ZMQEndpoint)
		if err != nil {
			log.Printf("ZMQ unavailable (%v) — falling back to RPC polling every %s",
				err, i.cfg.PollInterval)
		} else {
			log.Printf("ZMQ subscriber connected to %s", i.cfg.ZMQEndpoint)
			defer sub.Close()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		tip, err := i.retryGetBlockCount(ctx)
		if err != nil {
			return err
		}

		dbHeight, _, err := i.getDBTip(ctx)
		if err != nil {
			return err
		}

		lag := tip - dbHeight
		if lag > 0 {
			// ── Batch mode ────────────────────────────────────────────────
			// Ingest every block up to the daemon tip.  ingestRange handles
			// its own internal batching (BATCH_SIZE blocks per transaction).
			if lag > 1 {
				log.Printf("batch mode: %d blocks behind tip, ingesting %d -> %d",
					lag, dbHeight+1, tip)
			}
			if err := i.ingestRange(ctx, dbHeight+1, tip); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Transient node errors (503, work-queue full, etc.) mid-batch:
				// roll back already happened inside ingestRange; loop back to
				// retryGetBlockCount which applies its own exponential back-off
				// before we touch the node again.  This avoids the process
				// exiting (which would reset the backoff counter on restart).
				if isTransientRPCErr(err) {
					log.Printf("WARNING: ingestRange transient error: %v — backing off via retryGetBlockCount", err)
					continue
				}
				return err
			}
			// Re-check tip immediately — more blocks may have arrived.
			continue
		}

		// ── At tip: check 90-block hashrate boundary ─────────────────────
		// Per CERM v1: current_hashrate and peak_hashrate are defined over
		// 90-block (per-algo) daily windows.  We fire an update when any algo
		// has accumulated 90 new blocks since the last refresh.
		// lastHashrateCount starts at zero so the very first tip arrival always
		// triggers one update to populate the table from a clean state.
		i.checkAndUpdateHashrate(ctx)

		// ── Real-time mode ────────────────────────────────────────────────
		// Database is at the chain tip.  Block on the next ZMQ notification
		// instead of sleeping and polling, so we index each block within
		// milliseconds of it being accepted by bitmarkd.
		if sub != nil {
			log.Printf("at tip (%d) — waiting for ZMQ hashblock", tip)
			if err := sub.WaitForBlock(); err != nil {
				if err == zmq.ErrTimeout {
					// No block in 2 minutes; subscriber has reconnected.
					// Loop back and re-check the RPC tip before waiting again.
					log.Printf("ZMQ: %v", err)
					continue
				}
				// Unexpected ZMQ error; fall back to one poll cycle.
				log.Printf("ZMQ error: %v — polling once", err)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(i.cfg.PollInterval):
				}
				continue
			}
			// Brief pause so the RPC endpoint reflects the newly accepted block
			// before we call GetBlockCount again.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		} else {
			// ── Polling fallback (no ZMQ) ─────────────────────────────────
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(i.cfg.PollInterval):
			}
		}
	}
}

func (i *Ingestor) ingestRange(ctx context.Context, startHeight, endHeight int) error {
	if startHeight > endHeight {
		return nil
	}

	log.Printf("ingesting blocks %d -> %d (batch=%d)",
		startHeight, endHeight, i.cfg.BatchBlocks)

	batchStart := startHeight
	for batchStart <= endHeight {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		batchEnd := batchStart + i.cfg.BatchBlocks - 1
		if batchEnd > endHeight {
			batchEnd = endHeight
		}

		tx, err := i.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}

		lastHeight := -1
		lastHash := strings.Repeat("0", 64)

		for h := batchStart; h <= batchEnd; h++ {
			hash, err := i.rpc.GetBlockHash(ctx, h)
			if err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("getblockhash(%d): %w", h, err)
			}

			blk, err := i.rpc.GetBlockVerbose2(ctx, hash)
			if err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("getblock(%s,2): %w", hash, err)
			}

			if err := i.insertBlockBundle(ctx, tx, blk); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("insert height=%d hash=%s: %w", blk.Height, short(blk.Hash), err)
			}

			lastHeight = blk.Height
			lastHash = blk.Hash
		}

		if err := i.updateChainStateTx(ctx, tx, lastHeight, lastHash); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit batch %d-%d: %w", batchStart, batchEnd, err)
		}

		log.Printf("committed batch %d-%d (db tip now %d)", batchStart, batchEnd, lastHeight)
		batchStart = batchEnd + 1
	}

	return nil
}

func (i *Ingestor) insertBlockBundle(ctx context.Context, tx pgx.Tx, b *rpc.BlockV2) error {
	txCount := len(b.Tx)

	var blockTotalOutBU int64
	for _, t := range b.Tx {
		for _, v := range t.Vout {
			blockTotalOutBU += coinToBaseUnits(v.Value, i.cfg.BaseUnitFactor)
		}
	}

	// Decode algorithm and auxPoW flag from nVersion.
	// Bit 8 (0x100) of nVersion = merge-mined via auxPoW; bits 9-11 = algo ID.
	// We derive these locally from the raw version rather than trusting the
	// daemon's "auxpow" / "algo" JSON fields, which are omitempty and may be
	// absent when bitmarkd doesn't populate them.
	aID := decodeAlgo(int64(b.Version))
	aName := algoIDToName(aID)
	isAuxPow := decodeAuxPow(int64(b.Version)) || b.AuxPow
	auxPowSign := b.AuxPowSign
	if isAuxPow && auxPowSign == "" {
		auxPowSign = "merge-mined"
	}

	// Store weighted difficulty so it is comparable across algorithms
	// (matches the PHP rpcace page: difficulty × algoWeight).
	weightedDiff := b.Difficulty * algoWeight(aID)

	_, err := tx.Exec(ctx, `
INSERT INTO blocks (
  height, hash, prev_hash, merkle_root,
  time_utc, time_unix,
  version, bits, nonce, size_bytes, weight,
  difficulty, chainwork,
  tx_count, total_out, fees,
  algo_id, algo_name, auxpow, auxpow_sign,
  coreversion, in_best_chain
) VALUES (
  $1,$2,$3,$4,
  to_timestamp($5),$5,
  $6,$7,$8,$9,$10,
  $11,$12,
  $13,$14,$15,
  $16,$17,$18,$19,
  $20, TRUE
)
ON CONFLICT (hash) DO NOTHING
`,
		b.Height, b.Hash, nullIfEmpty(b.PreviousBlockHash), nullIfEmpty(b.MerkleRoot),
		b.Time,
		int64(b.Version), nullIfEmpty(b.Bits), int64(b.Nonce), b.Size, b.Weight,
		nullIfNaN(weightedDiff), nullIfEmpty(b.ChainWork),
		txCount, blockTotalOutBU, int64(0),
		aID, aName, isAuxPow, nullIfEmpty(auxPowSign),
		nullIfEmpty(b.CoreVersion),
	)
	if err != nil {
		return err
	}

	for pos, t := range b.Tx {
		isCoinbase := len(t.Vin) > 0 && t.Vin[0].Coinbase != ""

		var txTotalOutBU int64
		for _, v := range t.Vout {
			txTotalOutBU += coinToBaseUnits(v.Value, i.cfg.BaseUnitFactor)
		}

		_, err := tx.Exec(ctx, `
INSERT INTO transactions (
  txid, block_hash, block_height, position_in_block,
  version, locktime, size_bytes, vsize, weight,
  is_coinbase, total_in, total_out, fee
) VALUES (
  $1,$2,$3,$4,
  $5,$6,$7,$8,$9,
  $10,$11,$12,$13
)
ON CONFLICT (txid) DO NOTHING
`,
			t.Txid, b.Hash, b.Height, pos,
			intOrNil(t.Version), int64OrNil(t.Locktime), intOrNil(t.Size), intOrNil(t.Vsize), intOrNil(t.Weight),
			isCoinbase, int64(0), txTotalOutBU, int64(0),
		)
		if err != nil {
			return err
		}

		// Inputs
		for vinIdx, vin := range t.Vin {
			if vin.Coinbase != "" {
				_, err := tx.Exec(ctx, `
INSERT INTO tx_inputs (
  txid, vin, is_coinbase, coinbase_hex, sequence
) VALUES ($1,$2,TRUE,$3,$4)
ON CONFLICT (txid, vin) DO NOTHING
`,
					t.Txid, vinIdx, vin.Coinbase, int64OrNil(vin.Sequence),
				)
				if err != nil {
					return err
				}
				continue
			}

			_, err := tx.Exec(ctx, `
INSERT INTO tx_inputs (
  txid, vin, prev_txid, prev_vout, script_sig_hex, sequence,
  prev_value, prev_address, is_coinbase, coinbase_hex
) VALUES (
  $1,$2,$3,$4,$5,$6,
  NULL,NULL,FALSE,NULL
)
ON CONFLICT (txid, vin) DO NOTHING
`,
				t.Txid, vinIdx,
				nullIfEmpty(vin.Txid), intOrNil(vin.Vout),
				nullIfEmpty(vin.ScriptSig.Hex), int64OrNil(vin.Sequence),
			)
			if err != nil {
				return err
			}
		}

		// Outputs
		for _, vout := range t.Vout {
			valBU := coinToBaseUnits(vout.Value, i.cfg.BaseUnitFactor)
			_, err := tx.Exec(ctx, `
INSERT INTO tx_outputs (
  txid, vout, value, script_type, script_hex, address,
  spent_by_txid, spent_by_vin, spent_in_height
) VALUES (
  $1,$2,$3,$4,$5,$6,
  NULL,NULL,NULL
)
ON CONFLICT (txid, vout) DO NOTHING
`,
				t.Txid, int(vout.N),
				valBU,
				nullIfEmpty(vout.ScriptPubKey.Type),
				nullIfEmpty(vout.ScriptPubKey.Hex),
				nullIfEmpty(vout.ScriptPubKey.FirstAddress()),
			)
			if err != nil {
				return err
			}
		}

		// For non-coinbase inputs: look up prev output to fill prev_value/prev_address,
		// and mark the previous output as spent.
		if !isCoinbase {
			for vinIdx, vin := range t.Vin {
				if vin.Txid == "" {
					continue
				}
				var prevVal int64
				var prevAddr *string
				_ = tx.QueryRow(ctx,
					`SELECT value, address FROM tx_outputs WHERE txid=$1 AND vout=$2`,
					vin.Txid, vin.Vout,
				).Scan(&prevVal, &prevAddr)

				_, err := tx.Exec(ctx, `
UPDATE tx_inputs
SET prev_value=$1, prev_address=$2
WHERE txid=$3 AND vin=$4`,
					prevVal, prevAddr, t.Txid, vinIdx,
				)
				if err != nil {
					return err
				}

				_, err = tx.Exec(ctx, `
UPDATE tx_outputs
SET spent_by_txid=$1, spent_by_vin=$2, spent_in_height=$3
WHERE txid=$4 AND vout=$5`,
					t.Txid, vinIdx, b.Height, vin.Txid, vin.Vout,
				)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (i *Ingestor) ensureChainState(ctx context.Context) error {
	_, err := i.db.Exec(ctx, `
INSERT INTO chain_state (id, best_height, best_hash)
VALUES (1, -1, repeat('0', 64))
ON CONFLICT (id) DO NOTHING`)
	return err
}

func (i *Ingestor) getDBTip(ctx context.Context) (int, string, error) {
	var h int
	var hash string
	err := i.db.QueryRow(ctx, `SELECT best_height, best_hash FROM chain_state WHERE id=1`).Scan(&h, &hash)
	if err != nil {
		return -1, "", err
	}
	return h, hash, nil
}

func (i *Ingestor) updateChainStateTx(ctx context.Context, tx pgx.Tx, height int, hash string) error {
	_, err := tx.Exec(ctx, `
UPDATE chain_state
SET best_height=$1, best_hash=$2, updated_at=now()
WHERE id=1`, height, hash)
	return err
}

// --- Helper Functions ---

func decodeAlgo(version int64) int {
	return int((version >> 9) & 7)
}

// decodeAuxPow returns true when bit 8 (0x100) of nVersion is set,
// indicating the block was merge-mined via auxiliary proof-of-work.
func decodeAuxPow(version int64) bool {
	return version&0x100 != 0
}

// algoWeight returns the normalisation multiplier for a given algo ID.
// These match the weights used by the PHP rpcace page and the chaindynamics RPC.
// algo IDs: 0=SCRYPT, 1=SHA256D, 2=YESCRYPT, 3=ARGON2, 4=X17, 5=LYRA2REv2, 6=EQUIHASH, 7=CRYPTONIGHT
func algoWeight(id int) float64 {
	switch id {
	case 0:
		return 8_000      // SCRYPT
	case 1:
		return 1          // SHA256D (reference)
	case 2:
		return 800_000    // YESCRYPT
	case 3:
		return 4_000_000  // ARGON2
	case 4:
		return 8_000      // X17
	case 5:
		return 8_000      // LYRA2REv2
	case 6:
		return 8_000_000  // EQUIHASH
	case 7:
		return 8_000_000  // CRYPTONIGHT
	default:
		return 1
	}
}

func algoIDToName(id int) string {
	switch id {
	case 0:
		return "SCRYPT"
	case 1:
		return "SHA256D"
	case 2:
		return "YESCRYPT"
	case 3:
		return "ARGON2"
	case 4:
		return "X17"
	case 5:
		return "LYRA2REv2"
	case 6:
		return "EQUIHASH"
	case 7:
		return "CRYPTONIGHT"
	default:
		return "UNKNOWN"
	}
}

func coinToBaseUnits(v float64, factor int64) int64 {
	return int64(math.Round(v * float64(factor)))
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullIfNaN(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

func intOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func int64OrNil(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func short(h string) string {
	if len(h) <= 16 {
		return h
	}
	return h[:16] + "…"
}
