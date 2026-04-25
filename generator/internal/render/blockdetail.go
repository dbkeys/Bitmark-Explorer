package render

import (
	"fmt"
	"html/template"
	"io"
	"path/filepath"

	"github.com/dbkeys/bitmark-hp-gen/internal/db"
)

// RenderBlockDetail writes the block detail page to w.
func RenderBlockDetail(tplPath string, w io.Writer, b *db.BlockDetail) error {
	funcMap := template.FuncMap{
		// deref dereferences a *string, returning "" if nil
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		// derefInt dereferences a *int, returning 0 if nil
		"derefInt": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
		// slice8 returns the first 8 chars of a *string (for txid preview)
		"slice8": func(s *string) string {
			if s == nil {
				return ""
			}
			v := *s
			if len(v) > 8 {
				return v[:8]
			}
			return v
		},
		// inc / dec for next/prev block height labels
		"inc": func(n int64) int64 { return n + 1 },
		"dec": func(n int64) int64 { return n - 1 },
		// fmtInt formats an int64 with comma separators
		"fmtInt": func(n int64) string {
			s := []byte{}
			str := fmt.Sprintf("%d", n)
			for i, c := range str {
				if i > 0 && (len(str)-i)%3 == 0 {
					s = append(s, ',')
				}
				s = append(s, byte(c))
			}
			return string(s)
		},
	}

	base := filepath.Base(tplPath)
	tpl, err := template.New(base).Funcs(funcMap).ParseFiles(tplPath)
	if err != nil {
		return err
	}
	return tpl.Execute(w, b)
}
