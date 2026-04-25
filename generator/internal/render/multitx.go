package render

import (
	"fmt"
	"html/template"
	"io"
	"path/filepath"

	"github.com/dbkeys/bitmark-hp-gen/internal/db"
)

type MultiTxData struct {
	Blocks     []db.Block
	Total      int64
	Page       int
	TotalPages int
	PageNums   []int
}

func RenderMultiTx(tplPath string, w io.Writer, data MultiTxData) error {
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
		"slice": func(s string, i, j int) string {
			if i > len(s) {
				return s
			}
			if j > len(s) {
				j = len(s)
			}
			return s[i:j]
		},
		"derefStr": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		"inc": func(n int) int { return n + 1 },
		"dec": func(n int) int { return n - 1 },
		"gt":  func(a, b int) bool { return a > b },
		"ge":  func(a, b int) bool { return a >= b },
		"le":  func(a, b int) bool { return a <= b },
		"eq":  func(a, b int) bool { return a == b },
	}

	base := filepath.Base(tplPath)
	tpl, err := template.New(base).Funcs(funcMap).ParseFiles(tplPath)
	if err != nil {
		return err
	}
	return tpl.Execute(w, data)
}

// PageWindow returns up to 15 page numbers centred around current page.
func PageWindow(current, total int) []int {
	const window = 15
	start := current - window/2
	if start < 1 {
		start = 1
	}
	end := start + window - 1
	if end > total {
		end = total
		start = end - window + 1
		if start < 1 {
			start = 1
		}
	}
	nums := make([]int, 0, end-start+1)
	for i := start; i <= end; i++ {
		nums = append(nums, i)
	}
	return nums
}
