package ingest

import "time"

type Mode string

const (
	ModeBootstrap Mode = "BOOTSTRAP"
	ModeTail      Mode = "TAIL"
)

type Status struct {
	Mode Mode

	DaemonTip int
	SafeTip   int
	DBTip     int

	BatchStart int
	BatchEnd   int

	LastCommitHeight int
	LastCommitHash   string
	LastCommitAt     time.Time

	Message string
	Err     error
}

