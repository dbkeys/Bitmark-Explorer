// Package tui renders a live ncurses status display for the homepage generator.
// Redraw and Update must be called from the main goroutine; all other exported
// methods are safe for concurrent use.
package tui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/dbkeys/bitmark-hp-gen/internal/db"
	gc "github.com/rthornton128/goncurses"
)

// colour-pair IDs
const (
	pairHeader  int16 = 1
	pairColHead int16 = 2
	pairRowEven int16 = 3
	pairRowOdd  int16 = 4
	pairFooter  int16 = 5
	pairStatus  int16 = 6
	pairSep     int16 = 7
	pairLog     int16 = 8
	pairConn    int16 = 9  // connected-browsers header
	pairConnIP  int16 = 10 // per-IP rows
	pairNodeOK  int16 = 11 // node status: healthy
	pairNodeErr int16 = 12 // node status: error
)

// Layout constants
const (
	maxBlocks   = 12 // block rows shown
	maxIPRows   = 4  // IP rows shown (fixed height even when fewer are connected)
	minLogLines = 4  // minimum event log rows
	maxLogLines = 20 // maximum event log rows (extra terminal space fills here)
)

// TUI owns the ncurses screen.
type TUI struct {
	scr     *gc.Window
	blocks  []db.Block
	status  string
	lastGen time.Time
	outPath string

	mu          sync.Mutex
	events      []string // ring buffer — recent log messages
	connSummary string   // e.g. "3 conn · 2 tab · 2 IP"
	connLines   []string // per-IP display lines (up to maxIPRows)

	// Node / sync status (updated after each generate attempt)
	nodeStatusStr string // e.g. "Node: OK · height 2354102"
	nodeErrFlag   bool   // true when node RPC failed
	tipInfoStr    string // e.g. "DB tip: 2354102 · 23h 13m ago"

	forceCh chan struct{} // fires when the user presses R to force page regeneration
}

// New initialises ncurses and returns a ready TUI.  Call Close when done.
func New(outputPath string) (*TUI, error) {
	scr, err := gc.Init()
	if err != nil {
		return nil, fmt.Errorf("ncurses init: %w", err)
	}
	gc.Raw(true)
	gc.Echo(false)
	gc.Cursor(0)

	if gc.HasColors() {
		gc.StartColor()
		gc.UseDefaultColors()
		gc.InitPair(pairHeader,  gc.C_BLACK, gc.C_CYAN)  // title bar: dark on cyan
		gc.InitPair(pairColHead, gc.C_YELLOW, gc.C_BLACK) // column headers: yellow on black
		gc.InitPair(pairRowEven, gc.C_WHITE,  gc.C_BLACK) // even block rows: white on black
		gc.InitPair(pairRowOdd,  gc.C_WHITE,  gc.C_BLUE)  // odd block rows: white on blue (alternating)
		gc.InitPair(pairFooter,  gc.C_BLACK,  gc.C_WHITE) // footer: dark on white
		gc.InitPair(pairStatus,  gc.C_GREEN,  gc.C_BLACK) // status line: green on black
		gc.InitPair(pairSep,     gc.C_YELLOW, gc.C_BLACK) // separators: yellow on black
		gc.InitPair(pairLog,     gc.C_WHITE,  gc.C_BLACK) // log lines: white on black
		gc.InitPair(pairConn,    gc.C_BLACK,  gc.C_GREEN) // browsers header: dark on green
		gc.InitPair(pairConnIP,  gc.C_WHITE,  gc.C_BLACK) // per-IP rows: white on black
		gc.InitPair(pairNodeOK,  gc.C_BLACK,  gc.C_CYAN)  // node status OK: dark on cyan
		gc.InitPair(pairNodeErr, gc.C_WHITE,  gc.C_RED)   // node status ERR: white on red
	}

	scr.Keypad(true) // enable special-key codes
	scr.Timeout(0)   // non-blocking GetChar

	return &TUI{
		scr:           scr,
		status:        "Initializing...",
		outPath:       outputPath,
		connSummary:   "no browsers connected",
		nodeStatusStr: "Node: connecting...",
		tipInfoStr:    "",
		forceCh:       make(chan struct{}, 1),
	}, nil
}

// Close shuts down ncurses and restores the terminal.
func (t *TUI) Close() { gc.End() }

// LogWriter returns an io.Writer that writes to dst (a log file) and also
// captures each completed line into the TUI's event log section.
func (t *TUI) LogWriter(dst io.Writer) io.Writer {
	return &logWriter{tui: t, dst: dst}
}

type logWriter struct {
	tui *TUI
	dst io.Writer
}

func (w *logWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.dst != nil {
		w.dst.Write(p) //nolint:errcheck
	}
	line := strings.TrimRight(string(p), "\n\r")
	if line == "" {
		return n, nil
	}
	w.tui.mu.Lock()
	w.tui.events = append(w.tui.events, line)
	if len(w.tui.events) > 64 {
		w.tui.events = w.tui.events[len(w.tui.events)-64:]
	}
	w.tui.mu.Unlock()
	return n, nil
}

// SetStatus updates the status line. Thread-safe.
func (t *TUI) SetStatus(msg string) {
	t.mu.Lock()
	t.status = msg
	t.mu.Unlock()
	t.Redraw()
}

// Update stores the latest block list and generation timestamp, then redraws.
func (t *TUI) Update(blocks []db.Block, generatedAt time.Time) {
	t.blocks = blocks
	t.lastGen = generatedAt
	t.Redraw()
}

// SetClients updates the connected-browsers section.
//   - conns, tabs, ips: aggregate counts
//   - lines: per-IP display strings (pre-formatted by the caller); at most maxIPRows shown
//
// Thread-safe; does NOT trigger a redraw — the next ticker redraw picks it up.
func (t *TUI) SetClients(conns, tabs, ips int, lines []string) {
	var summary string
	if conns == 0 {
		summary = "no browsers connected"
	} else {
		summary = fmt.Sprintf("%d conn · %d tab · %d IP", conns, tabs, ips)
	}
	t.mu.Lock()
	t.connSummary = summary
	if len(lines) > maxIPRows {
		t.connLines = lines[:maxIPRows]
	} else {
		t.connLines = lines
	}
	t.mu.Unlock()
}

// SetNodeStatus updates the node-RPC and sync-status row.
//   - nodeStatusStr: pre-formatted node info, e.g. "Node: OK · height 2354102"
//   - nodeErr: true when the node RPC is unavailable or returning errors
//   - tipInfoStr: pre-formatted DB-tip info, e.g. "DB tip: 2354102 · 23h ago"
//
// Thread-safe; does NOT trigger a redraw.
func (t *TUI) SetNodeStatus(nodeStatusStr string, nodeErr bool, tipInfoStr string) {
	t.mu.Lock()
	t.nodeStatusStr = nodeStatusStr
	t.nodeErrFlag = nodeErr
	t.tipInfoStr = tipInfoStr
	t.mu.Unlock()
}

// ForceCh returns the channel that fires when the user presses R to force
// a page regeneration.  Callers should select on this channel.
func (t *TUI) ForceCh() <-chan struct{} {
	return t.forceCh
}

// CheckInput reads any pending keystrokes (non-blocking).  Pressing R/r sends
// a signal on ForceCh.  Must be called from the main goroutine.
func (t *TUI) CheckInput() {
	for {
		ch := t.scr.GetChar()
		if ch <= 0 {
			return
		}
		switch rune(ch) {
		case 'r', 'R':
			select {
			case t.forceCh <- struct{}{}:
			default:
			}
		}
	}
}

// Redraw repaints the entire screen from the current state.
// Must be called from the main goroutine.
func (t *TUI) Redraw() {
	rows, cols := t.scr.MaxYX()
	if rows < 20 || cols < 40 {
		return
	}

	// ── Dynamic log-line count ────────────────────────────────────────────────
	// Fixed rows consumed by non-log sections:
	//   header(1) + colhdr(1) + sep(1)
	//   + maxBlocks(12) block rows
	//   + sep(1) + connHdr(1) + maxIPRows(4) + sep(1)
	//   + eventsHdr(1)
	//   + sep(1) + nodeStatus(1) + footer(1) + status(1)
	// = 27 fixed rows
	const fixedRows = 27
	dynLogLines := rows - fixedRows
	if dynLogLines < minLogLines {
		dynLogLines = minLogLines
	}
	if dynLogLines > maxLogLines {
		dynLogLines = maxLogLines
	}

	// Erase (NOT Clear) fills ncurses' internal buffer with spaces but does NOT
	// set the clearok flag.  The subsequent Refresh sends only a differential
	// update — no full-screen blank flash.  Combined with every row being padded
	// to exactly `cols` bytes, ghost content from earlier renders is impossible.
	t.scr.Erase()

	// ── Layout ───────────────────────────────────────────────────────────────
	// Row 0          header bar (title + clock)
	// Row 1          column headers
	// Row 2          separator
	// Rows 3..14     maxBlocks block rows (fixed 12)
	// Row 15         separator
	// Row 16         "Connected Browsers" header
	// Rows 17..20    maxIPRows IP rows (fixed 4, blank when fewer connected)
	// Row 21         separator
	// Row 22         "Recent Events" label
	// Rows 23..22+L  dynLogLines event rows (expands to fill extra terminal space)
	// Row 23+L       separator
	// Row 24+L       node/sync status row  (NEW)
	// Row 25+L       footer bar (output path + last generated)
	// Row 26+L       status line (never write last cell — avoids scroll)

	row := 0

	t.drawRow(row, cols, pairHeader, true, t.headerLine(cols)); row++
	t.drawRow(row, cols, pairColHead, true, t.colHdrLine(cols)); row++
	t.drawSep(row, cols); row++

	for i := 0; i < maxBlocks; i++ {
		pair := pairRowEven
		if i%2 != 0 {
			pair = pairRowOdd
		}
		t.drawRow(row, cols, pair, false, t.blockLine(i, cols))
		row++
	}

	t.drawSep(row, cols); row++

	// Connected Browsers section
	t.mu.Lock()
	summary := t.connSummary
	clines := make([]string, len(t.connLines))
	copy(clines, t.connLines)
	t.mu.Unlock()

	t.drawRow(row, cols, pairConn, true,
		pad(fmt.Sprintf("  Connected Browsers — %s", summary), cols))
	row++

	for i := 0; i < maxIPRows; i++ {
		line := ""
		if i < len(clines) {
			line = clines[i]
		}
		t.drawRow(row, cols, pairConnIP, false, pad(line, cols))
		row++
	}

	t.drawSep(row, cols); row++

	// Recent Events section
	t.drawRow(row, cols, pairColHead, true, pad("  Recent Events", cols)); row++

	t.mu.Lock()
	evs := t.events
	if len(evs) > 64 {
		evs = evs[len(evs)-64:]
	}
	evsCopy := make([]string, len(evs))
	copy(evsCopy, evs)
	t.mu.Unlock()

	start := len(evsCopy) - dynLogLines
	if start < 0 {
		start = 0
	}
	for i := 0; i < dynLogLines; i++ {
		line := ""
		if idx := start + i; idx < len(evsCopy) {
			line = "  " + evsCopy[idx]
		}
		t.drawRow(row, cols, pairLog, false, pad(line, cols))
		row++
	}

	t.drawSep(row, cols); row++

	// Node / sync status row
	t.mu.Lock()
	nodeErr := t.nodeErrFlag
	t.mu.Unlock()
	nodePair := pairNodeOK
	if nodeErr {
		nodePair = pairNodeErr
	}
	t.drawRow(row, cols, nodePair, nodeErr, t.nodeStatusLine(cols)); row++

	// Footer
	t.drawRow(row, cols, pairFooter, false, t.footerLine(cols)); row++

	// Status — one cell short on the last line to prevent terminal scroll
	t.mu.Lock()
	status := t.status
	t.mu.Unlock()
	t.drawRow(row, cols-1, pairStatus, false, t.statusLine(status, cols-1))

	t.scr.Refresh()
}

// ── private drawing helpers ──────────────────────────────────────────────────

func (t *TUI) drawRow(row, cols int, pair int16, bold bool, line string) {
	t.scr.ColorOn(pair)
	if bold {
		t.scr.AttrOn(gc.A_BOLD)
	}
	t.scr.MovePrint(row, 0, line)
	if bold {
		t.scr.AttrOff(gc.A_BOLD)
	}
	t.scr.ColorOff(pair)
}

func (t *TUI) drawSep(row, cols int) {
	t.scr.ColorOn(pairSep)
	for x := 0; x < cols; x++ {
		t.scr.MoveAddChar(row, x, gc.ACS_HLINE)
	}
	t.scr.ColorOff(pairSep)
}

// ── line builders ────────────────────────────────────────────────────────────

func (t *TUI) headerLine(cols int) string {
	left := " BITMARK EXPLORER  *  Homepage Generator"
	right := " " + time.Now().UTC().Format("2006-01-02  15:04:05 UTC") + " "
	return padBetween(left, right, cols)
}

func (t *TUI) colHdrLine(cols int) string {
	s := fmt.Sprintf("  %-9s %-20s %-12s %14s %5s  %-19s",
		"Height", "Hash (prefix)", "Algorithm", "Difficulty", "Tx#", "Time (UTC)")
	return pad(s, cols)
}

func (t *TUI) blockLine(i, cols int) string {
	if i >= len(t.blocks) {
		return strings.Repeat(" ", cols)
	}
	b := t.blocks[i]
	algo := "unknown"
	if b.AlgoName != nil {
		algo = *b.AlgoName
	}
	hash := b.Hash
	if len(hash) > 19 {
		hash = hash[:18] + "~"
	}
	s := fmt.Sprintf("  %-9d %-20s %-12s %14.4f %5d  %s",
		b.Height, hash, algo, b.Difficulty,
		b.TxCount, b.TimeUTC.UTC().Format("2006-01-02 15:04:05"))
	return pad(s, cols)
}

func (t *TUI) nodeStatusLine(cols int) string {
	t.mu.Lock()
	ns := t.nodeStatusStr
	ts := t.tipInfoStr
	t.mu.Unlock()
	return padBetween("  "+ns, ts+"  ", cols)
}

func (t *TUI) footerLine(cols int) string {
	genStr := "not yet generated"
	if !t.lastGen.IsZero() {
		genStr = t.lastGen.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	left := fmt.Sprintf("  Output: %s", t.outPath)
	right := fmt.Sprintf("  Last generated: %s  ", genStr)
	return padBetween(left, right, cols)
}

func (t *TUI) statusLine(status string, cols int) string {
	left := fmt.Sprintf("  >> %s", status)
	right := " [R] regenerate  " + time.Now().UTC().Format("15:04:05") + " "
	return pad(padBetween(left, right, cols), cols)
}

// ── string utilities ─────────────────────────────────────────────────────────

// pad returns s padded to exactly n bytes (spaces), or truncated if longer.
func pad(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}

// padBetween places left and right on one line of width cols with spaces between.
func padBetween(left, right string, cols int) string {
	need := len(left) + len(right)
	if need >= cols {
		return pad(left, cols)
	}
	return left + strings.Repeat(" ", cols-need) + right
}
