package render

import (
	"fmt"
	"html/template"
	"io"
	"path/filepath"

	"github.com/dbkeys/bitmark-hp-gen/internal/db"
)

// RenderAddressDetail writes the address detail page to w.
func RenderAddressDetail(tplPath string, w io.Writer, a *db.AddressDetail) error {
	funcMap := template.FuncMap{
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
		"fmtSats": func(n int64) string {
			// Format satoshis as BTM with 8 decimal places
			return fmt.Sprintf("%.8f", float64(n)/1e8)
		},
		"positive": func(n int64) bool { return n > 0 },
		"negative": func(n int64) bool { return n < 0 },
		"slice": func(s string, i, j int) string {
			if i > len(s) {
				return s
			}
			if j > len(s) {
				j = len(s)
			}
			return s[i:j]
		},
	}

	base := filepath.Base(tplPath)
	tpl, err := template.New(base).Funcs(funcMap).ParseFiles(tplPath)
	if err != nil {
		return err
	}
	return tpl.Execute(w, a)
}
