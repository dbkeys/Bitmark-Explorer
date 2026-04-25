package ui

import (
	"fmt"
	"time"

	gc "github.com/rthornton128/goncurses"

	"github.com/bitmark/bitmark-indexer/internal/ingest"
)

type TUI struct {
	stdscr   *gc.Window
	started  time.Time
	progName string
	version  string
}

func New(progName, version string) *TUI {
	return &TUI{
		started:  time.Now(),
		progName: progName,
		version:  version,
	}
}

func (t *TUI) Init() error {
	stdscr, err := gc.Init()
	if err != nil {
		return err
	}
	t.stdscr = stdscr
	gc.CBreak(true)
	gc.Echo(false)
	gc.Cursor(0)
	gc.StartColor()
	gc.UseDefaultColors()

	// Basic color pairs (optional; safe in most terms)
	_ = gc.InitPair(1, gc.C_WHITE, -1)  // header
	_ = gc.InitPair(2, gc.C_GREEN, -1)  // OK
	_ = gc.InitPair(3, gc.C_YELLOW, -1) // warn
	_ = gc.InitPair(4, gc.C_RED, -1)    // err

	t.stdscr.NoutRefresh()
	gc.Update()
	return nil
}

func (t *TUI) Close() {
	if t.stdscr != nil {
		gc.End()
	}
}

func (t *TUI) Render(s ingest.Status) {
	if t.stdscr == nil {
		return
	}

	h, w := t.stdscr.MaxYX()
	t.stdscr.Erase()

	// Header
	t.stdscr.AttrOn(gc.ColorPair(1))
	header := fmt.Sprintf("%s %s", t.progName, t.version)
	t.put(0, 0, padRight(header, w))
	t.stdscr.AttrOff(gc.ColorPair(1))

	// Mode line
	uptime := time.Since(t.started).Round(time.Second)
	modeLine := fmt.Sprintf("Mode: %-9s  Uptime: %s", s.Mode, uptime)
	t.put(1, 0, padRight(modeLine, w))

	// Heights/progress
	progress := fmt.Sprintf("DB Tip: %d   Safe Tip: %d   Daemon Tip: %d", s.DBTip, s.SafeTip, s.DaemonTip)
	t.put(2, 0, padRight(progress, w))

	// Batch
	batch := fmt.Sprintf("Batch: %d -> %d   Last Commit: %d @ %s",
		s.BatchStart, s.BatchEnd, s.LastCommitHeight, formatTime(s.LastCommitAt))
	t.put(3, 0, padRight(batch, w))

	// Message / error (last line-ish)
	msgY := 5
	if msgY < h {
		if s.Err != nil {
			t.stdscr.AttrOn(gc.ColorPair(4))
			t.put(msgY, 0, padRight("ERROR: "+s.Err.Error(), w))
			t.stdscr.AttrOff(gc.ColorPair(4))
		} else if s.Message != "" {
			t.stdscr.AttrOn(gc.ColorPair(2))
			t.put(msgY, 0, padRight(s.Message, w))
			t.stdscr.AttrOff(gc.ColorPair(2))
		}
	}

	// Footer hint
	if h-1 >= 0 {
		t.stdscr.AttrOn(gc.ColorPair(3))
		t.put(h-1, 0, padRight("Ctrl+C to stop", w))
		t.stdscr.AttrOff(gc.ColorPair(3))
	}

	t.stdscr.NoutRefresh()
	gc.Update()
}

func (t *TUI) put(y, x int, s string) {
	_ = t.stdscr.MovePrint(y, x, s)
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}
	return s + makeSpaces(width-len(s))
}

func makeSpaces(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%*s", n, "")
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

