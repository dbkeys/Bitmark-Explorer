package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bitmark/bitmark-indexer/internal/config"
	"github.com/bitmark/bitmark-indexer/internal/db"
	"github.com/bitmark/bitmark-indexer/internal/ingest"
	"github.com/bitmark/bitmark-indexer/internal/rpc"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Println("shutdown signal received")
		cancel()
	}()

	pool, err := db.Connect(ctx, cfg.PostgresDSN)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer pool.Close()

	rpcClient := rpc.NewClient(cfg.RPC)

	ing := ingest.New(cfg, pool, rpcClient)

	if err := ing.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("indexer error: %v", err)
	}

	log.Println("indexer stopped cleanly")
}

