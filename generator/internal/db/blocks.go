package db

import (
	"context"
)

const MultiTxPageSize = 50

// MultiTxBlocks returns blocks with more than one transaction, newest first,
// with pagination. page is 1-based. Also returns the total count.
func (d *DB) MultiTxBlocks(ctx context.Context, page int) ([]Block, int64, error) {
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * MultiTxPageSize

	var total int64
	if err := d.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM blocks WHERE in_best_chain = TRUE AND tx_count > 1
	`).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := d.Pool.Query(ctx, `
		SELECT
			height, hash, difficulty, time_utc,
			tx_count, total_out, algo_id, algo_name,
			version, coreversion, auxpow
		FROM blocks
		WHERE in_best_chain = TRUE AND tx_count > 1
		ORDER BY height DESC
		LIMIT $1 OFFSET $2
	`, MultiTxPageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var blocks []Block
	for rows.Next() {
		var b Block
		if err := rows.Scan(
			&b.Height, &b.Hash, &b.Difficulty, &b.TimeUTC,
			&b.TxCount, &b.TotalOut, &b.AlgoID, &b.AlgoName,
			&b.Version, &b.CoreVersion, &b.Auxpow,
		); err != nil {
			return nil, 0, err
		}
		b.TotalOutBTM = float64(b.TotalOut) / 1e8
		blocks = append(blocks, b)
	}
	return blocks, total, nil
}

/*
LatestBlocks returns the most recent blocks
from the best chain.
*/
func (d *DB) LatestBlocks(ctx context.Context, limit int) ([]Block, error) {

	rows, err := d.Pool.Query(ctx, `
		SELECT
			height,
			hash,
			difficulty,
			time_utc,
			tx_count,
			total_out,
			algo_id,
			algo_name,
			version,
			coreversion,
			auxpow
		FROM blocks
		WHERE in_best_chain = TRUE
		ORDER BY height DESC
		LIMIT $1
	`, limit)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blocks []Block

	for rows.Next() {
		var b Block

		err := rows.Scan(
			&b.Height,
			&b.Hash,
			&b.Difficulty,
			&b.TimeUTC,
			&b.TxCount,
			&b.TotalOut,
			&b.AlgoID,
			&b.AlgoName,
			&b.Version,
			&b.CoreVersion,
			&b.Auxpow,
		)

		if err != nil {
			return nil, err
		}

		// Precompute BTM value for template
		b.TotalOutBTM = float64(b.TotalOut) / 100000000.0

		blocks = append(blocks, b)
	}

	return blocks, nil
}
