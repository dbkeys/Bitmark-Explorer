package db

import (
	"context"
)

// AlgoStatInput holds the per-algo values sourced from RPC calls.
type AlgoStatInput struct {
	AlgoID             int
	AlgoName           string
	Difficulty         float64
	Hashrate           float64
	PeakHashrate       float64
	MoneySupply        float64 // BTM
	BlockReward        float64 // BTM (SSF-scaled current reward)
	NominalBlockReward float64 // BTM (unscaled epoch reward)
	NSSFBlocks         int
	BlockSpacing       float64 // minutes
}

// UpsertAlgoStats inserts or updates algo_stats_current from RPC-sourced data.
// peak_hashrate is kept as the running maximum (never decreased).
func (d *DB) UpsertAlgoStats(ctx context.Context, stats []AlgoStatInput) error {
	for _, s := range stats {
		moneySats := int64(s.MoneySupply * 1e8)
		_, err := d.Pool.Exec(ctx, `
			INSERT INTO algo_stats_current
				(algo_id, algo_name, difficulty, hashrate, peak_hashrate,
				 money_supply, block_reward, nominal_block_reward, nssf_remaining, block_spacing, computed_at)
			VALUES ($1,$2,$3,$4,$4,$5,$6,$7,$8,$9,NOW())
			ON CONFLICT (algo_id) DO UPDATE SET
				algo_name           = EXCLUDED.algo_name,
				difficulty          = EXCLUDED.difficulty,
				hashrate            = EXCLUDED.hashrate,
				peak_hashrate       = GREATEST(algo_stats_current.peak_hashrate, EXCLUDED.peak_hashrate),
				money_supply        = EXCLUDED.money_supply,
				block_reward        = EXCLUDED.block_reward,
				nominal_block_reward = EXCLUDED.nominal_block_reward,
				nssf_remaining      = EXCLUDED.nssf_remaining,
				block_spacing       = EXCLUDED.block_spacing,
				computed_at         = NOW()
		`, s.AlgoID, s.AlgoName, s.Difficulty, s.Hashrate, moneySats,
			s.BlockReward, s.NominalBlockReward, s.NSSFBlocks, s.BlockSpacing)
		if err != nil {
			return err
		}
	}
	return nil
}
