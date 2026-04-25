package db

import (
	"context"
)

// AlgoStats reads the current per-algorithm statistics from algo_stats_current.
func (d *DB) AlgoStats(ctx context.Context) ([]AlgoStat, error) {
	rows, err := d.Pool.Query(ctx, `
        SELECT
            algo_id,
            algo_name,
            difficulty,
            hashrate,
            peak_hashrate,
            money_supply,
            block_reward,
            nominal_block_reward,
            nssf_remaining,
            block_spacing,
            computed_at
        FROM algo_stats_current
        ORDER BY algo_id
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []AlgoStat

	for rows.Next() {
		var s AlgoStat
		if err := rows.Scan(
			&s.AlgoID,
			&s.AlgoName,
			&s.Difficulty,
			&s.Hashrate,
			&s.PeakHashrate,
			&s.MoneySupply,
			&s.BlockReward,
			&s.NominalBlockReward,
			&s.NSSFRemaining,
			&s.BlockSpacing,
			&s.ComputedAt,
		); err != nil {
			return nil, err
		}
		s.MoneySupplyBTM = float64(s.MoneySupply) / 1e8
		stats = append(stats, s)
	}

	return stats, nil
}

// GlobalStats returns chain-level aggregate statistics.
func (d *DB) GlobalStats(ctx context.Context) (GlobalStats, error) {
	var gs GlobalStats

	// Best block height (≈ block count displayed by getblockcount)
	if err := d.Pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(height), 0)
		FROM blocks
		WHERE in_best_chain = TRUE
	`).Scan(&gs.NumBlocks); err != nil {
		return gs, err
	}

	// Total money supply across all algorithms (satoshis → BTM)
	if err := d.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(money_supply), 0) / 100000000.0
		FROM algo_stats_current
	`).Scan(&gs.MoneySupply); err != nil {
		return gs, err
	}

	// Average block spacing (minutes) computed from the last 100 block intervals
	if err := d.Pool.QueryRow(ctx, `
		SELECT COALESCE(
			EXTRACT(EPOCH FROM (MAX(time_utc) - MIN(time_utc)))
			/ NULLIF(COUNT(*) - 1, 0) / 60.0,
			0.0
		)
		FROM (
			SELECT time_utc
			FROM blocks
			WHERE in_best_chain = TRUE
			ORDER BY height DESC
			LIMIT 101
		) sub
	`).Scan(&gs.BlockSpacing); err != nil {
		return gs, err
	}

	return gs, nil
}

// RefreshAlgoStats populates or updates algo_stats_current.
// On first call (table empty) it runs a full scan — this takes ~30 seconds on a large chain.
// On subsequent calls it does a fast incremental update.
func (d *DB) RefreshAlgoStats(ctx context.Context) error {
	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM algo_stats_current`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return d.fullComputeAlgoStats(ctx)
	}
	return d.incrementalUpdateAlgoStats(ctx)
}

// fullComputeAlgoStats does a one-time full computation from the blocks and transactions tables.
// Money-supply is computed by summing all coinbase outputs per algo (~30s on a 2M-block chain).
func (d *DB) fullComputeAlgoStats(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, `
		INSERT INTO algo_stats_current
			(algo_id, algo_name, difficulty, hashrate, peak_hashrate,
			 money_supply, block_reward, nssf_remaining, block_spacing, computed_at)
		WITH
		latest_diff AS (
			SELECT DISTINCT ON (algo_id)
				algo_id, algo_name, difficulty
			FROM blocks
			WHERE in_best_chain = TRUE AND algo_id IS NOT NULL
			ORDER BY algo_id, height DESC
		),
		spacing AS (
			SELECT algo_id, AVG(gap_s) AS avg_s
			FROM (
				SELECT algo_id,
					EXTRACT(EPOCH FROM (
						time_utc - LAG(time_utc) OVER (PARTITION BY algo_id ORDER BY height)
					)) AS gap_s
				FROM blocks
				WHERE in_best_chain = TRUE AND algo_id IS NOT NULL
				  AND height >= (SELECT MAX(height) - 2000 FROM blocks WHERE in_best_chain = TRUE)
			) sub
			WHERE gap_s > 0
			GROUP BY algo_id
		),
		money AS (
			SELECT b.algo_id, SUM(t.total_out) AS supply_sats
			FROM transactions t
			JOIN blocks b ON b.hash = t.block_hash
			WHERE t.is_coinbase = TRUE AND b.in_best_chain = TRUE AND b.algo_id IS NOT NULL
			GROUP BY b.algo_id
		),
		-- Use LATERAL to force per-algo index lookups instead of a full scan + DISTINCT
		latest_block_per_algo AS (
			SELECT b.algo_id, b.hash
			FROM (VALUES (0),(1),(2),(3),(4),(5),(6),(7)) AS ids(aid)
			CROSS JOIN LATERAL (
				SELECT algo_id, hash FROM blocks
				WHERE in_best_chain = TRUE AND algo_id = ids.aid
				ORDER BY height DESC LIMIT 1
			) b
		),
		reward AS (
			SELECT lb.algo_id, t.total_out AS reward_sats
			FROM latest_block_per_algo lb
			JOIN transactions t ON t.block_hash = lb.hash AND t.is_coinbase = TRUE
		)
		SELECT
			ld.algo_id,
			ld.algo_name,
			COALESCE(ld.difficulty, 0),
			-- hashrate in Gh/s: difficulty × 2^32 / avg_spacing_seconds / 1e9
			COALESCE(CASE WHEN s.avg_s > 0
				THEN ld.difficulty * 4294967296.0 / s.avg_s / 1e9
				ELSE 0 END, 0),
			COALESCE(CASE WHEN s.avg_s > 0
				THEN ld.difficulty * 4294967296.0 / s.avg_s / 1e9
				ELSE 0 END, 0),
			COALESCE(m.supply_sats, 0),
			COALESCE(r.reward_sats / 1e8, 0),
			0,
			COALESCE(s.avg_s / 60.0, 0),
			NOW()
		FROM latest_diff ld
		LEFT JOIN spacing s ON s.algo_id = ld.algo_id
		LEFT JOIN money   m ON m.algo_id = ld.algo_id
		LEFT JOIN reward  r ON r.algo_id = ld.algo_id
		ORDER BY ld.algo_id
	`)
	return err
}

// incrementalUpdateAlgoStats does a fast per-block update:
// it recalculates difficulty, hashrate, block_spacing, and block_reward from recent blocks,
// and adds only the newly-mined coinbase outputs to money_supply.
func (d *DB) incrementalUpdateAlgoStats(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, `
		WITH
		latest_diff AS (
			SELECT DISTINCT ON (algo_id) algo_id, difficulty
			FROM blocks WHERE in_best_chain = TRUE AND algo_id IS NOT NULL
			ORDER BY algo_id, height DESC
		),
		spacing AS (
			SELECT algo_id, AVG(gap_s) AS avg_s
			FROM (
				SELECT algo_id,
					EXTRACT(EPOCH FROM (
						time_utc - LAG(time_utc) OVER (PARTITION BY algo_id ORDER BY height)
					)) AS gap_s
				FROM blocks
				WHERE in_best_chain = TRUE AND algo_id IS NOT NULL
				  AND height >= (SELECT MAX(height) - 2000 FROM blocks WHERE in_best_chain = TRUE)
			) sub
			WHERE gap_s > 0
			GROUP BY algo_id
		),
		new_coins AS (
			SELECT b.algo_id, COALESCE(SUM(t.total_out), 0) AS new_sats
			FROM blocks b
			JOIN transactions t ON t.block_hash = b.hash AND t.is_coinbase = TRUE
			WHERE b.in_best_chain = TRUE AND b.algo_id IS NOT NULL
			  AND b.time_utc > (SELECT MIN(computed_at) FROM algo_stats_current)
			GROUP BY b.algo_id
		),
		-- Use LATERAL to force per-algo index lookups instead of a full scan + DISTINCT
		latest_block_per_algo AS (
			SELECT b.algo_id, b.hash
			FROM (VALUES (0),(1),(2),(3),(4),(5),(6),(7)) AS ids(aid)
			CROSS JOIN LATERAL (
				SELECT algo_id, hash FROM blocks
				WHERE in_best_chain = TRUE AND algo_id = ids.aid
				ORDER BY height DESC LIMIT 1
			) b
		),
		latest_reward AS (
			SELECT lb.algo_id, t.total_out AS reward_sats
			FROM latest_block_per_algo lb
			JOIN transactions t ON t.block_hash = lb.hash AND t.is_coinbase = TRUE
		)
		UPDATE algo_stats_current a
		SET
			difficulty    = ld.difficulty,
			hashrate      = CASE WHEN s.avg_s > 0
				THEN ld.difficulty * 4294967296.0 / s.avg_s / 1e9
				ELSE a.hashrate END,
			peak_hashrate = GREATEST(a.peak_hashrate,
				CASE WHEN s.avg_s > 0
				THEN ld.difficulty * 4294967296.0 / s.avg_s / 1e9
				ELSE 0 END),
			money_supply  = a.money_supply + COALESCE(nc.new_sats, 0),
			block_reward  = CASE WHEN lr.reward_sats IS NOT NULL
				THEN lr.reward_sats / 1e8
				ELSE a.block_reward END,
			block_spacing = COALESCE(s.avg_s / 60.0, a.block_spacing),
			computed_at   = NOW()
		FROM latest_diff ld
		LEFT JOIN spacing       s  ON s.algo_id  = ld.algo_id
		LEFT JOIN new_coins     nc ON nc.algo_id = ld.algo_id
		LEFT JOIN latest_reward lr ON lr.algo_id = ld.algo_id
		WHERE a.algo_id = ld.algo_id
	`)
	return err
}
