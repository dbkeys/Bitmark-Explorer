// backfill-addresses populates tx_outputs.address from script_hex for P2PKH
// and P2SH outputs, and updates tx_inputs.prev_address / prev_value by joining
// against tx_outputs.  Run once after upgrading the indexer to populate
// historical rows that were ingested before address decoding was added.
//
// Usage:
//
//	PG_DSN="postgres://..." backfill-addresses [--batch N]
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Bitmark mainnet version bytes (from kernel/chainparams.cpp)
const (
	versionP2PKH byte = 85 // 0x55 — produces addresses starting with 'b'
	versionP2SH  byte = 5  // 0x05
)

var base58Alphabet = []byte("123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz")

func base58Check(version byte, hash20 []byte) string {
	full := make([]byte, 21)
	full[0] = version
	copy(full[1:], hash20)

	h1 := sha256.Sum256(full)
	h2 := sha256.Sum256(h1[:])
	full = append(full, h2[:4]...)

	n := new(big.Int).SetBytes(full)
	zero := new(big.Int)
	mod := new(big.Int)
	b58 := big.NewInt(58)
	var result []byte
	for n.Cmp(zero) > 0 {
		n.DivMod(n, b58, mod)
		result = append(result, base58Alphabet[mod.Int64()])
	}
	for _, b := range full {
		if b != 0 {
			break
		}
		result = append(result, base58Alphabet[0])
	}
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return string(result)
}

// decodeScriptAddress returns the address for P2PKH and P2SH scripts, or "".
func decodeScriptAddress(scriptType, scriptHex string) string {
	raw, err := hex.DecodeString(scriptHex)
	if err != nil {
		return ""
	}
	switch scriptType {
	case "pubkeyhash":
		// 76 a9 14 <20-byte hash160> 88 ac
		if len(raw) == 25 && raw[0] == 0x76 && raw[1] == 0xa9 && raw[2] == 0x14 && raw[23] == 0x88 && raw[24] == 0xac {
			return base58Check(versionP2PKH, raw[3:23])
		}
	case "scripthash":
		// a9 14 <20-byte hash160> 87
		if len(raw) == 23 && raw[0] == 0xa9 && raw[1] == 0x14 && raw[22] == 0x87 {
			return base58Check(versionP2SH, raw[2:22])
		}
	}
	return ""
}

func main() {
	batchFlag := flag.Int("batch", 10000, "rows per UPDATE batch")
	flag.Parse()

	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		log.Fatal("PG_DSN not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	log.Println("Phase 1: decoding addresses from script_hex into tx_outputs...")
	if err := backfillOutputAddresses(ctx, pool, *batchFlag); err != nil {
		log.Fatalf("output address backfill: %v", err)
	}

	log.Println("Phase 2: propagating addresses into tx_inputs.prev_address / prev_value...")
	if err := backfillInputAddresses(ctx, pool); err != nil {
		log.Fatalf("input address backfill: %v", err)
	}

	log.Println("Done.")
}

func backfillOutputAddresses(ctx context.Context, pool *pgxpool.Pool, batchSize int) error {
	rows, err := pool.Query(ctx, `
		SELECT txid, vout, script_type, script_hex
		FROM tx_outputs
		WHERE address IS NULL
		  AND script_type IS NOT NULL
		  AND script_hex  IS NOT NULL
		  AND script_type IN ('pubkeyhash','scripthash')
	`)
	if err != nil {
		return err
	}

	type row struct{ txid, vout, addr string }
	var batch []row
	total := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		for _, r := range batch {
			if _, err := tx.Exec(ctx,
				`UPDATE tx_outputs SET address=$1 WHERE txid=$2 AND vout=$3`,
				r.addr, r.txid, r.vout,
			); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		total += len(batch)
		log.Printf("  updated %d output rows (total %d)", len(batch), total)
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		var txid, stype, shex string
		var vout int
		if err := rows.Scan(&txid, &vout, &stype, &shex); err != nil {
			continue
		}
		addr := decodeScriptAddress(stype, shex)
		if addr == "" {
			continue
		}
		batch = append(batch, row{txid, fmt.Sprintf("%d", vout), addr})
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				rows.Close()
				return err
			}
		}
	}
	rows.Close()
	return flush()
}

func backfillInputAddresses(ctx context.Context, pool *pgxpool.Pool) error {
	res, err := pool.Exec(ctx, `
		UPDATE tx_inputs ti
		SET
			prev_value   = o.value,
			prev_address = o.address
		FROM tx_outputs o
		WHERE ti.prev_txid = o.txid
		  AND ti.prev_vout = o.vout
		  AND ti.is_coinbase = FALSE
		  AND (ti.prev_value IS NULL OR ti.prev_address IS NULL)
		  AND o.address IS NOT NULL
	`)
	if err != nil {
		return err
	}
	log.Printf("  updated %d input rows", res.RowsAffected())
	return nil
}
