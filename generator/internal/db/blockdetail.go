package db

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// BlockByQuery looks up a block by:
//   - numeric string  → block height
//   - 64-char hex     → block hash, or (if not found) txid of a containing block
//
// Returns nil, nil when nothing matches.
func (d *DB) BlockByQuery(ctx context.Context, q string) (*BlockDetail, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}

	if height, err := strconv.ParseInt(q, 10, 64); err == nil {
		return d.blockByHeight(ctx, height)
	}

	if len(q) == 64 {
		b, err := d.blockByHash(ctx, q)
		if err != nil {
			return nil, err
		}
		if b != nil {
			return b, nil
		}
		return d.blockByTxid(ctx, q)
	}

	return nil, nil
}

func (d *DB) blockByHash(ctx context.Context, hash string) (*BlockDetail, error) {
	return d.loadBlock(ctx, "WHERE hash = $1 AND in_best_chain = TRUE", hash)
}

func (d *DB) blockByHeight(ctx context.Context, height int64) (*BlockDetail, error) {
	return d.loadBlock(ctx, "WHERE height = $1 AND in_best_chain = TRUE", height)
}

func (d *DB) blockByTxid(ctx context.Context, txid string) (*BlockDetail, error) {
	var blockHash string
	err := d.Pool.QueryRow(ctx,
		`SELECT block_hash FROM transactions WHERE txid = $1`, txid,
	).Scan(&blockHash)
	if err != nil {
		return nil, nil
	}
	return d.blockByHash(ctx, strings.TrimSpace(blockHash))
}

func (d *DB) loadBlock(ctx context.Context, where string, arg interface{}) (*BlockDetail, error) {
	var b BlockDetail
	var algoID *int16 // smallint from DB

	err := d.Pool.QueryRow(ctx, `
		SELECT
			height, hash, prev_hash, merkle_root,
			time_utc, time_unix,
			version, coreversion, bits, nonce,
			size_bytes, weight,
			difficulty, chainwork,
			tx_count, total_out,
			algo_id, algo_name, auxpow, auxpow_sign
		FROM blocks
		`+where, arg,
	).Scan(
		&b.Height, &b.Hash, &b.PrevHash, &b.MerkleRoot,
		&b.TimeUTC, &b.TimeUnix,
		&b.Version, &b.CoreVersion, &b.Bits, &b.Nonce,
		&b.SizeBytes, &b.Weight,
		&b.Difficulty, &b.Chainwork,
		&b.TxCount, &b.TotalOut,
		&algoID, &b.AlgoName, &b.Auxpow, &b.AuxpowSign,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// CHAR(64) columns come back space-padded — trim them all.
	b.Hash = strings.TrimSpace(b.Hash)
	trimPtr(&b.PrevHash)
	trimPtr(&b.MerkleRoot)

	if algoID != nil {
		id := int(*algoID)
		b.AlgoID = &id
	}
	b.TotalOutBTM = float64(b.TotalOut) / 1e8

	// Next block hash (may not exist yet for the tip)
	var nextHash string
	if err := d.Pool.QueryRow(ctx,
		`SELECT hash FROM blocks WHERE height = $1 AND in_best_chain = TRUE`,
		b.Height+1,
	).Scan(&nextHash); err == nil {
		s := strings.TrimSpace(nextHash)
		b.NextHash = &s
	}

	// Confirmations = tip_height − block_height + 1
	var tip int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(height), 0) FROM blocks WHERE in_best_chain = TRUE`,
	).Scan(&tip); err == nil {
		b.Confirmations = tip - b.Height + 1
	}

	// Transactions in block order
	txRows, err := d.Pool.Query(ctx, `
		SELECT txid, is_coinbase, COALESCE(total_out, 0)
		FROM transactions
		WHERE block_hash = $1
		ORDER BY position_in_block
	`, b.Hash)
	if err == nil {
		defer txRows.Close()
		for txRows.Next() {
			var tx TxDetail
			var totalSats int64
			if scanErr := txRows.Scan(&tx.Txid, &tx.IsCoinbase, &totalSats); scanErr != nil {
				continue
			}
			tx.Txid = strings.TrimSpace(tx.Txid)
			tx.TotalOut = totalSats
			tx.TotalOutBTM = float64(totalSats) / 1e8
			b.Txs = append(b.Txs, tx)
		}
		txRows.Close()
	}

	// Inputs and outputs for each transaction
	for i := range b.Txs {
		b.Txs[i].Inputs, _ = d.txInputs(ctx, b.Txs[i].Txid)
		b.Txs[i].Outputs, _ = d.txOutputs(ctx, b.Txs[i].Txid)
	}

	return &b, nil
}

func (d *DB) txInputs(ctx context.Context, txid string) ([]TxInputDetail, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT prev_txid, prev_vout, prev_address, COALESCE(prev_value, 0),
		       is_coinbase, coinbase_hex
		FROM tx_inputs
		WHERE txid = $1
		ORDER BY vin
	`, txid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var inputs []TxInputDetail
	for rows.Next() {
		var inp TxInputDetail
		var val int64
		if err := rows.Scan(
			&inp.PrevTxid, &inp.PrevVout, &inp.PrevAddr, &val,
			&inp.IsCoinbase, &inp.CoinbaseHex,
		); err != nil {
			continue
		}
		inp.PrevVal = val
		inp.PrevValBTM = float64(val) / 1e8
		trimPtr(&inp.PrevTxid)
		inputs = append(inputs, inp)
	}
	return inputs, nil
}

func (d *DB) txOutputs(ctx context.Context, txid string) ([]TxOutputDetail, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT vout, value, script_type, address
		FROM tx_outputs
		WHERE txid = $1
		ORDER BY vout
	`, txid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var outputs []TxOutputDetail
	for rows.Next() {
		var out TxOutputDetail
		var val int64
		if err := rows.Scan(&out.Vout, &val, &out.ScriptType, &out.Address); err != nil {
			continue
		}
		out.Val = val
		out.ValBTM = float64(val) / 1e8
		outputs = append(outputs, out)
	}
	return outputs, nil
}

// trimPtr trims trailing spaces from a *string in place (for CHAR columns).
func trimPtr(s **string) {
	if *s != nil {
		t := strings.TrimSpace(**s)
		*s = &t
	}
}
