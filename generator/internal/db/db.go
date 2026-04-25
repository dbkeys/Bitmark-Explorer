package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) Close() {
	d.Pool.Close()
}

// BestHeight returns the indexed chain tip height from chain_state.
// Returns -1 if chain_state is empty or unavailable.
func (d *DB) BestHeight(ctx context.Context) (int, error) {
	var h int
	err := d.Pool.QueryRow(ctx, `SELECT best_height FROM chain_state WHERE id=1`).Scan(&h)
	if err != nil {
		return -1, err
	}
	return h, nil
}
