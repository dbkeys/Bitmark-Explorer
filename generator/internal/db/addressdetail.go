package db

import (
	"context"
	"strings"
)

// AddressDetail looks up an address and returns its full history, or nil if
// the address has no transactions in the DB.
func (d *DB) AddressDetail(ctx context.Context, addr string) (*AddressDetail, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, nil
	}

	// Summary stats
	var detail AddressDetail
	detail.Address = addr

	err := d.Pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(value), 0)                                    AS total_recv,
			COALESCE(SUM(value) FILTER (WHERE spent_by_txid IS NOT NULL), 0) AS total_spent
		FROM tx_outputs
		WHERE address = $1
	`, addr).Scan(&detail.TotalRecvSats, &detail.TotalSentSats)
	if err != nil {
		return nil, err
	}

	// Nothing found for this address
	if detail.TotalRecvSats == 0 && detail.TotalSentSats == 0 {
		// Double-check there are no inputs either
		var inputCount int
		_ = d.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM tx_inputs WHERE prev_address=$1`, addr,
		).Scan(&inputCount)
		if inputCount == 0 {
			return nil, nil
		}
	}

	detail.BalanceSats = detail.TotalRecvSats - detail.TotalSentSats
	detail.BalanceBTM = float64(detail.BalanceSats) / 1e8
	detail.TotalRecvBTM = float64(detail.TotalRecvSats) / 1e8
	detail.TotalSentBTM = float64(detail.TotalSentSats) / 1e8

	// Transaction history with net amount per tx
	rows, err := d.Pool.Query(ctx, `
		WITH output_txs AS (
			SELECT txid, SUM(value) AS received
			FROM tx_outputs
			WHERE address = $1
			GROUP BY txid
		),
		input_txs AS (
			SELECT txid, SUM(prev_value) AS spent
			FROM tx_inputs
			WHERE prev_address = $1
			GROUP BY txid
		),
		all_txs AS (
			SELECT
				COALESCE(o.txid, i.txid)  AS txid,
				COALESCE(o.received, 0)   AS received,
				COALESCE(i.spent, 0)      AS spent
			FROM output_txs o
			FULL OUTER JOIN input_txs i ON o.txid = i.txid
		)
		SELECT
			a.txid,
			t.block_height,
			b.time_utc,
			(a.received - a.spent) AS net_sats
		FROM all_txs a
		JOIN transactions t ON t.txid = a.txid
		JOIN blocks b ON b.hash = t.block_hash AND b.in_best_chain = TRUE
		ORDER BY t.block_height ASC, t.position_in_block ASC
	`, addr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runningBalance int64
	for rows.Next() {
		var row AddressTxRow
		if err := rows.Scan(&row.Txid, &row.BlockHeight, &row.TimeUTC, &row.NetSats); err != nil {
			continue
		}
		row.Txid = strings.TrimSpace(row.Txid)
		row.NetBTM = float64(row.NetSats) / 1e8
		runningBalance += row.NetSats
		row.BalanceSats = runningBalance
		row.BalanceBTM = float64(runningBalance) / 1e8
		detail.Txs = append(detail.Txs, row)
	}
	detail.TxCount = len(detail.Txs)

	return &detail, nil
}
