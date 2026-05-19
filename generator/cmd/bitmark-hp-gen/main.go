package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/dbkeys/bitmark-hp-gen/internal/db"
	"github.com/dbkeys/bitmark-hp-gen/internal/render"
	noderpc "github.com/dbkeys/bitmark-hp-gen/internal/rpc"
	"github.com/dbkeys/bitmark-hp-gen/internal/sse"
	"github.com/dbkeys/bitmark-hp-gen/internal/tui"
	appzmq "github.com/dbkeys/bitmark-hp-gen/internal/zmq"
)

func mustEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx := context.Background()

	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		log.Fatal("PG_DSN not set")
	}

	templatePath   := mustEnv("TEMPLATE_PATH", "/var/www/templates/bitmark-hp-gen/homepage.html")
	blockTplPath   := mustEnv("BLOCK_TEMPLATE_PATH", "/var/www/templates/bitmark-hp-gen/blockdetail.html")
	addrTplPath    := mustEnv("ADDR_TEMPLATE_PATH", "/var/www/templates/bitmark-hp-gen/addressdetail.html")
	multitxTplPath := mustEnv("MULTITX_TEMPLATE_PATH", "/var/www/templates/bitmark-hp-gen/multitx.html")
	outputPath     := mustEnv("OUTPUT_PATH", "")
	staticDir      := mustEnv("STATIC_DIR", "")
	if outputPath == "" {
		log.Fatal("OUTPUT_PATH not set — set EXPLORER_DOMAIN and re-run install.sh")
	}
	if staticDir == "" {
		log.Fatal("STATIC_DIR not set — set EXPLORER_DOMAIN and re-run install.sh")
	}
	zmqEndpoint    := mustEnv("ZMQ_ENDPOINT", "tcp://127.0.0.1:28332")
	listenAddr     := mustEnv("LISTEN_ADDR", "127.0.0.1:8088")
	rpcURL         := mustEnv("RPC_URL", "")
	rpcUser        := mustEnv("RPC_USER", "")
	rpcPass        := mustEnv("RPC_PASS", "")

	database, err := db.New(ctx, dsn)
	if err != nil {
		log.Fatalf("DB connection error: %v", err)
	}
	defer database.Close()

	// Optional RPC client for node height queries (used for sync status banner).
	// If RPC is not configured the banner falls back to tip-age estimation.
	var rpc *noderpc.Client
	if rpcURL != "" && rpcUser != "" && rpcPass != "" {
		rpc = noderpc.NewWithTimeout(rpcURL, rpcUser, rpcPass, 3*time.Second)
	}

	sub, err := appzmq.NewSubscriber(zmqEndpoint)
	if err != nil {
		log.Fatalf("ZMQ subscriber error: %v", err)
	}

	ui, err := tui.New(outputPath)
	if err != nil {
		log.Fatalf("TUI init error: %v (is TERM set? run inside tmux)", err)
	}
	defer ui.Close()

	logPath := mustEnv("LOG_PATH", "/var/log/bitmark-hp-gen.log")
	if lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		log.SetOutput(ui.LogWriter(lf))
		defer lf.Close()
	} else {
		log.SetOutput(ui.LogWriter(nil))
	}

	broker := sse.NewBroker()
	go startHTTPServer(listenAddr, staticDir, templatePath, blockTplPath, addrTplPath, multitxTplPath, database, broker)

	blockCh  := make(chan struct{}, 1)
	zmqErrCh := make(chan error, 1)
	go func() {
		for {
			if err := sub.WaitForBlock(); err != nil {
				zmqErrCh <- err
			} else {
				select {
				case blockCh <- struct{}{}:
				default:
				}
			}
		}
	}()

	// State for sync-status tracking across renders.
	var (
		lastRenderedHeight = -1
		lastRenderedAt     time.Time
		ingestRate         float64 // blocks per second observed between renders
	)

	breaker := &rpcBreaker{}

	doGenerate := func(reason string) {
		ui.SetStatus(reason)
		nodeHeight, nodeAvail, nodeErrMsg := breaker.query(rpc)
		h := generate(ctx, database, ui, templatePath, outputPath, nodeHeight, nodeAvail, nodeErrMsg, ingestRate)
		// Update ingest-rate estimate using progress since last render.
		now := time.Now()
		if h > lastRenderedHeight && lastRenderedHeight >= 0 && !lastRenderedAt.IsZero() {
			if secs := now.Sub(lastRenderedAt).Seconds(); secs > 0 {
				ingestRate = float64(h-lastRenderedHeight) / secs
			}
		}
		if h >= 0 {
			lastRenderedHeight = h
			lastRenderedAt = now
		}
	}

	doGenerate("Generating initial homepage...")

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	tickCount := 0

	for {
		select {
		case <-blockCh:
			time.Sleep(time.Second) // brief pause for indexer to commit
			doGenerate("New block — regenerating...")
			broker.Broadcast()

		case err := <-zmqErrCh:
			if err == appzmq.ErrTimeout {
				ui.SetStatus(fmt.Sprintf("WARNING: %v — reconnected, waiting...", err))
			} else {
				ui.SetStatus(fmt.Sprintf("ZMQ error: %v", err))
			}

		case <-ui.ForceCh():
			doGenerate("Manual regeneration requested...")
			broker.Broadcast()

		case <-ticker.C:
			tickCount++
			conns, tabs, ips, byIP := broker.Connected()
			lines := make([]string, len(byIP))
			for i, s := range byIP {
				lines[i] = fmt.Sprintf("  · %-24s  %d conn  %d tab", s.IP, s.Conns, s.Tabs)
			}
			ui.SetClients(conns, tabs, ips, lines)
			ui.CheckInput()
			ui.SetStatus("Waiting for new block...")
			ui.Redraw()

			// Every 30 seconds: poll DB for missed ZMQ notifications.
			if tickCount%30 == 0 {
				if h, err := database.BestHeight(ctx); err == nil && h > lastRenderedHeight {
					doGenerate(fmt.Sprintf("DB advanced to %d (missed ZMQ) — regenerating...", h))
					broker.Broadcast()
				}
			}
		}
	}
}

// rpcBreaker is a simple circuit breaker / exponential-backoff guard for the
// node RPC.  After each consecutive failure the next call is suppressed for an
// increasing duration (up to rpcBackoffMax), so an overloaded node is not
// hammered on every generate cycle.  A single success resets the state.
//
// Not safe for concurrent use — call only from the main goroutine.
type rpcBreaker struct {
	failures  int
	nextTryAt time.Time
	lastErr   string
}

// rpcBackoffDelays lists the wait durations (in seconds) after 1, 2, 3, …
// consecutive failures.  The last entry is reused indefinitely.
var rpcBackoffDelays = []time.Duration{
	0,          // 1st failure: retry immediately on the next generate cycle
	5,          // 2nd
	15,         // 3rd
	30,         // 4th
	60,         // 5th  (1 min)
	120,        // 6th  (2 min)
	300,        // 7th  (5 min)
	600,        // 8th+ (10 min)
}

// query calls GetBlockCount on the node, honouring the backoff schedule.
// Returns (height, available, errorMessage).
func (b *rpcBreaker) query(c *noderpc.Client) (int, bool, string) {
	if c == nil {
		return 0, false, "RPC not configured"
	}
	// During back-off: skip the call and return the cached error.
	if remaining := time.Until(b.nextTryAt); remaining > 0 {
		msg := fmt.Sprintf("%s (retry in %s)", b.lastErr, remaining.Round(time.Second))
		return 0, false, msg
	}
	n, err := c.GetBlockCount()
	if err != nil {
		b.failures++
		idx := b.failures
		if idx >= len(rpcBackoffDelays) {
			idx = len(rpcBackoffDelays) - 1
		}
		delay := rpcBackoffDelays[idx]
		if delay > 0 {
			b.nextTryAt = time.Now().Add(delay * time.Second)
		}
		msg := err.Error()
		if len(msg) > 80 {
			msg = msg[:77] + "..."
		}
		b.lastErr = msg
		return 0, false, err.Error()
	}
	// Success: reset breaker.
	b.failures = 0
	b.nextTryAt = time.Time{}
	b.lastErr = ""
	return n, true, ""
}

// buildNodeStr returns a pre-formatted node-RPC status string for the TUI.
func buildNodeStr(avail bool, height int, errMsg string) string {
	if avail {
		return fmt.Sprintf("Node: OK · height %d", height)
	}
	if errMsg == "" || errMsg == "RPC not configured" {
		return "Node: " + errMsg
	}
	// Truncate long error messages so the status row stays on one line.
	if len(errMsg) > 55 {
		errMsg = errMsg[:52] + "..."
	}
	return "Node: ERR · " + errMsg
}

// buildTipStr returns a pre-formatted DB-tip status string for the TUI.
func buildTipStr(blocks []db.Block) string {
	if len(blocks) == 0 {
		return "DB tip: no blocks indexed"
	}
	tipAge := time.Since(blocks[0].TimeUTC)
	if tipAge < time.Minute {
		return fmt.Sprintf("DB tip: %d · synced", blocks[0].Height)
	}
	return fmt.Sprintf("DB tip: %d · %s ago", blocks[0].Height, fmtAge(tipAge))
}

// fmtAge formats a duration as a human-readable string ("3 minutes", "1h 12m", etc.).
func fmtAge(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		s := int(d.Seconds())
		if s == 1 {
			return "1 second"
		}
		return fmt.Sprintf("%d seconds", s)
	}
	if d < time.Hour {
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", m)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		if h == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

// buildSyncInfo computes the sync-status banner data for the homepage template.
func buildSyncInfo(blocks []db.Block, nodeHeight int, nodeAvail bool, ingestRate float64) render.SyncInfo {
	if len(blocks) == 0 {
		return render.SyncInfo{}
	}

	tipTime := blocks[0].TimeUTC
	tipAge  := time.Since(tipTime)

	var lag int
	var catching bool

	if nodeAvail && nodeHeight > 0 {
		lag      = nodeHeight - int(blocks[0].Height)
		catching = lag > 5
	} else {
		// No node height available — use tip age as a proxy.
		// With ~1 block/minute across 8 algos, > 3 minutes implies we're behind.
		catching = tipAge > 3*time.Minute
	}

	if !catching {
		return render.SyncInfo{}
	}

	si := render.SyncInfo{
		Catching:      true,
		Lag:           lag,
		NodeAvailable: nodeAvail,
		TipTimeStr:    tipTime.UTC().Format("2006-Jan-02 15:04 UTC"),
		TipAgeStr:     fmtAge(tipAge),
	}

	// Estimate catch-up time from observed ingest rate and remaining lag.
	if ingestRate > 0 && lag > 0 {
		eta := time.Duration(float64(lag)/ingestRate) * time.Second
		if eta < 10*time.Second {
			si.ETAStr = "moments"
		} else {
			si.ETAStr = "~" + fmtAge(eta)
		}
	}

	return si
}

// generate fetches data from the DB, builds the homepage, and writes it to disk.
// Returns the DB tip height, or -1 on error.
func generate(ctx context.Context, d *db.DB, ui *tui.TUI, templatePath, outputPath string,
	nodeHeight int, nodeAvail bool, nodeErrMsg string, ingestRate float64) int {

	blocks, err := d.LatestBlocks(ctx, 24)
	if err != nil {
		ui.SetStatus(fmt.Sprintf("DB error (blocks): %v", err))
		return -1
	}

	algoStats, err := d.AlgoStats(ctx)
	if err != nil {
		ui.SetStatus(fmt.Sprintf("DB error (algo stats): %v", err))
		return -1
	}

	globalStats, err := d.GlobalStats(ctx)
	if err != nil {
		ui.SetStatus(fmt.Sprintf("DB error (global stats): %v", err))
		return -1
	}

	// Update TUI node/sync status now that we have all the data.
	ui.SetNodeStatus(
		buildNodeStr(nodeAvail, nodeHeight, nodeErrMsg),
		!nodeAvail,
		buildTipStr(blocks),
	)

	now := time.Now().UTC()
	syncInfo := buildSyncInfo(blocks, nodeHeight, nodeAvail, ingestRate)

	totalPages := int((globalStats.NumBlocks + int64(db.HomePageSize) - 1) / int64(db.HomePageSize))
	err = render.GenerateHomepage(
		templatePath,
		outputPath,
		render.HomepageData{
			Blocks:      blocks,
			AlgoStats:   algoStats,
			GlobalStats: globalStats,
			GeneratedAt: now,
			Sync:        syncInfo,
			Page:        1,
			TotalPages:  totalPages,
			PrevPage:    0,
			NextPage:    2,
		},
	)
	if err != nil {
		ui.SetStatus(fmt.Sprintf("Render error: %v", err))
		return -1
	}

	ui.Update(blocks, now)
	ui.SetStatus("Waiting for new block...")

	if len(blocks) > 0 {
		return int(blocks[0].Height)
	}
	return -1
}

func startHTTPServer(addr, staticDir, homepageTplPath, blockTplPath, addrTplPath, multitxTplPath string, database *db.DB, broker *sse.Broker) {
	fileServer := http.FileServer(http.Dir(staticDir))

	mux := http.NewServeMux()
	mux.Handle("/events", broker)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		ctx := r.Context()

		// Paginated older-blocks view: /?page=N (N >= 2 served dynamically)
		if pageStr := r.URL.Query().Get("page"); pageStr != "" && q == "" && r.URL.Query().Get("view") == "" {
			var page int
			fmt.Sscanf(pageStr, "%d", &page)
			if page >= 2 {
				blocks, total, err := database.PagedBlocks(ctx, page)
				if err != nil {
					http.Error(w, "database error", http.StatusInternalServerError)
					return
				}
				algoStats, _ := database.AlgoStats(ctx)
				globalStats, _ := database.GlobalStats(ctx)
				totalPages := int((total + int64(db.HomePageSize) - 1) / int64(db.HomePageSize))
				prevPage := page - 1
				nextPage := 0
				if page < totalPages {
					nextPage = page + 1
				}
				data := render.HomepageData{
					Blocks:      blocks,
					AlgoStats:   algoStats,
					GlobalStats: globalStats,
					GeneratedAt: time.Now().UTC(),
					Page:        page,
					TotalPages:  totalPages,
					PrevPage:    prevPage,
					NextPage:    nextPage,
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				if err := render.RenderHomepage(homepageTplPath, w, data); err != nil {
					log.Printf("render blocks page error: %v", err)
				}
				return
			}
		}

		if r.URL.Query().Get("view") == "multitx" {
			page := 1
			if p := r.URL.Query().Get("page"); p != "" {
				fmt.Sscanf(p, "%d", &page)
			}
			blocks, total, err := database.MultiTxBlocks(ctx, page)
			if err != nil {
				http.Error(w, "database error", http.StatusInternalServerError)
				return
			}
			totalPages := int((total + int64(db.MultiTxPageSize) - 1) / int64(db.MultiTxPageSize))
			data := render.MultiTxData{
				Blocks:     blocks,
				Total:      total,
				Page:       page,
				TotalPages: totalPages,
				PageNums:   render.PageWindow(page, totalPages),
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := render.RenderMultiTx(multitxTplPath, w, data); err != nil {
				log.Printf("render multitx error: %v", err)
			}
			return
		}

		if q == "" {
			fileServer.ServeHTTP(w, r)
			return
		}

		block, err := database.BlockByQuery(ctx, q)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		if block != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := render.RenderBlockDetail(blockTplPath, w, block); err != nil {
				log.Printf("render block detail error: %v", err)
			}
			return
		}

		addrDetail, err := database.AddressDetail(ctx, q)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		if addrDetail != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := render.RenderAddressDetail(addrTplPath, w, addrDetail); err != nil {
				log.Printf("render address detail error: %v", err)
			}
			return
		}

		http.NotFound(w, r)
	})

	log.Printf("HTTP server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}
