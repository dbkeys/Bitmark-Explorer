package render

import (
	"html/template"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dbkeys/bitmark-hp-gen/internal/db"
//	"github.com/dbkeys/mPoW-Explorer-MP-Gen/internal/db"
)

// SyncInfo is non-zero when the explorer is behind the chain tip.
// All display strings are pre-formatted so the template stays simple.
type SyncInfo struct {
	Catching      bool   // true → show the banner
	Lag           int    // blocks behind (0 when NodeAvailable is false)
	NodeAvailable bool   // false while the node is loading or unreachable
	TipTimeStr    string // e.g. "2026-Apr-08 14:43 UTC"
	TipAgeStr     string // e.g. "32 minutes"
	ETAStr        string // e.g. "~2 minutes", empty when unknown
}

type HomepageData struct {
	GeneratedAt time.Time
	Blocks      []db.Block
	AlgoStats   []db.AlgoStat
	GlobalStats db.GlobalStats
	Sync        SyncInfo
}

func GenerateHomepage(tplPath, outputPath string, data HomepageData) error {
	funcMap := template.FuncMap{
		// poolURL returns the mining pool URL for a given algorithm name, or ""
		"poolURL": func(algoName string) string {
			switch algoName {
			case "SHA256D", "EQUIHASH":
				return "https://pool.chainetics.com"
			case "YESCRYPT", "ARGON2", "X17":
				return "https://pool.openmarks.com"
			default:
				return ""
			}
		},
		// fmtInt formats an int64 with comma thousands separators
		"fmtInt": func(n int64) string {
			s := strconv.FormatInt(n, 10)
			if len(s) <= 3 {
				return s
			}
			var b []byte
			for i, c := range s {
				if i > 0 && (len(s)-i)%3 == 0 {
					b = append(b, ',')
				}
				b = append(b, byte(c))
			}
			return string(b)
		},
		// fmtBTM rounds a float64 BTM value to an integer and formats with commas
		"fmtBTM": func(f float64) string {
			n := int64(f + 0.5) // round
			s := strconv.FormatInt(n, 10)
			if len(s) <= 3 {
				return s
			}
			var b []byte
			for i, c := range s {
				if i > 0 && (len(s)-i)%3 == 0 {
					b = append(b, ',')
				}
				b = append(b, byte(c))
			}
			return string(b)
		},
		// fmtF0 rounds a float64 to the nearest integer and formats with comma separators
		"fmtF0": func(f float64) string {
			n := int64(f + 0.5)
			s := strconv.FormatInt(n, 10)
			if len(s) <= 3 {
				return s
			}
			var b []byte
			for i, c := range s {
				if i > 0 && (len(s)-i)%3 == 0 {
					b = append(b, ',')
				}
				b = append(b, byte(c))
			}
			return string(b)
		},
		// fmtF2 formats a float64 to 2 decimal places with comma separators in the integer part
		"fmtF2": func(f float64) string {
			// Split into integer and fractional parts
			n := int64(f)
			frac := f - float64(n)
			if frac < 0 {
				frac = -frac
			}
			s := strconv.FormatInt(n, 10)
			var b []byte
			for i, c := range s {
				if i > 0 && (len(s)-i)%3 == 0 {
					b = append(b, ',')
				}
				b = append(b, byte(c))
			}
			decimals := strconv.FormatFloat(frac, 'f', 2, 64)
			return string(b) + decimals[1:] // append ".xx"
		},
	}

	base := filepath.Base(tplPath)
	tpl, err := template.New(base).Funcs(funcMap).ParseFiles(tplPath)
	if err != nil {
		return err
	}

	tmp := outputPath + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	if err := tpl.Execute(f, data); err != nil {
		f.Close()
		return err
	}
	f.Close()

	return os.Rename(tmp, outputPath)
}
