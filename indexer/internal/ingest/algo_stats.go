package ingest

import (
	"context"
	"fmt"
	"log"
	"math"
)

// refreshAlgoStats refreshes all per-algo stats from the node on every new block.
func (i *Ingestor) refreshAlgoStats(ctx context.Context) {
	if err := i.doUpdateAlgoStats(ctx); err != nil {
		log.Printf("WARNING: algo stats update: %v", err)
	}
}

// doUpdateAlgoStats fetches live values from the node RPC and upserts
// algo_stats_current for all 8 algorithms.
//
// hashrate  = node's current 90-block daily average (per CERM v1 §1)
// peak_hashrate = node's highest 90-block average over the trailing year (CERM v1 §3)
// Both come from chaindynamics; we store them as-is — no GREATEST override.
func (i *Ingestor) doUpdateAlgoStats(ctx context.Context) error {
	cd, err := i.rpc.ChainDynamics(ctx)
	if err != nil {
		return fmt.Errorf("chaindynamics: %w", err)
	}

	// Block spacing comes directly from chaindynamics in minutes — the node
	// computes the average time between consecutive same-algo blocks over its
	// own internal window (aligned with the CERM 90-block daily interval).
	for idx := 0; idx < 8; idx++ {
		ms, err := i.rpc.MoneySupply(ctx, idx)
		if err != nil {
			return fmt.Errorf("getmoneysupply(%d): %w", idx, err)
		}
		br, err := i.rpc.BlockReward(ctx, idx)
		if err != nil {
			return fmt.Errorf("getblockreward(%d): %w", idx, err)
		}

		// The node's getblockreward(algoID, -1, true) does not return the
		// unscaled nominal — it returns the same SSF-adjusted value.
		// Derive the nominal from the inverse CERM formula instead:
		//   block_reward = (nominal/2) × (1 + ESF)
		//   nominal = block_reward × 2 / (1 + ESF)
		// then snap to the nearest value in the known Bitmark epoch schedule.
		nbr := nominalEpochReward(br, cd.Hashrate[idx], cd.PeakHashrate[idx])

		moneySats := int64(ms * 1e8)
		_, err = i.db.Exec(ctx, `
			INSERT INTO algo_stats_current
				(algo_id, algo_name, difficulty, hashrate, peak_hashrate,
				 money_supply, block_reward, nominal_block_reward,
				 nssf_remaining, block_spacing, computed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NOW())
			ON CONFLICT (algo_id) DO UPDATE SET
				algo_name            = EXCLUDED.algo_name,
				difficulty           = EXCLUDED.difficulty,
				hashrate             = EXCLUDED.hashrate,
				peak_hashrate        = EXCLUDED.peak_hashrate,
				money_supply         = EXCLUDED.money_supply,
				block_reward         = EXCLUDED.block_reward,
				nominal_block_reward = EXCLUDED.nominal_block_reward,
				nssf_remaining       = EXCLUDED.nssf_remaining,
				block_spacing        = EXCLUDED.block_spacing,
				computed_at          = NOW()
		`, idx, algoIDToName(idx),
			cd.Difficulty[idx], cd.Hashrate[idx], cd.PeakHashrate[idx],
			moneySats, br, nbr,
			cd.NSSFBlocks[idx], cd.BlockSpacing[idx])
		if err != nil {
			return fmt.Errorf("upsert algo_stats_current(algo=%d): %w", idx, err)
		}
	}
	return nil
}

// nominalEpochReward derives the epoch nominal block reward using the inverse
// CERM v1 formula:
//
//	block_reward = (nominal/2) × (1 + ESF)   where ESF = hashrate/peak_hashrate
//	            → nominal = block_reward × 2 / (1 + ESF)
//
// The raw result is snapped to the nearest value in Bitmark's known epoch
// schedule (20, 15, 10, 7.5, 5, 3.75, 2.5, 1.875, 1.25, …) which alternates
// between ×3/4 (quartering) and ×2/3 (halving) steps starting from 20.
func nominalEpochReward(blockReward, hashrate, peakHashrate float64) float64 {
	// Build the epoch table far enough to cover any realistic chain age.
	// Pattern: ×3/4, ×2/3, ×3/4, ×2/3, … starting from 20.
	epochs := make([]float64, 0, 30)
	v := 20.0
	for v >= 1e-6 {
		epochs = append(epochs, v)
		if len(epochs)%2 == 1 {
			v *= 3.0 / 4.0 // quartering step
		} else {
			v *= 2.0 / 3.0 // halving step
		}
	}

	// Compute ESF, clamped to [0, 1].
	esf := 0.0
	if peakHashrate > 0 {
		esf = hashrate / peakHashrate
		if esf > 1 {
			esf = 1
		}
	}

	// Inverse CERM: nominal = block_reward × 2 / (1 + ESF)
	raw := 0.0
	if denom := 1 + esf; denom > 0 {
		raw = blockReward * 2 / denom
	}

	// Snap to nearest epoch value.
	best := epochs[0]
	for _, e := range epochs[1:] {
		if math.Abs(raw-e) < math.Abs(raw-best) {
			best = e
		}
	}
	return best
}
