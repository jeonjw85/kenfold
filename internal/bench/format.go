package bench

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

func parseLoCoMoDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	t, err := time.Parse("3:04 pm on 2 January, 2006", s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func parseLMEDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '('); i >= 0 {
		if j := strings.IndexByte(s[i:], ')'); j >= 0 {
			s = strings.TrimSpace(s[:i] + " " + s[i+j+1:])
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	t, err := time.Parse("2006/01/02 15:04", s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func clip(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// maxContentBytes keeps a memory inside the btree limit of memory_dedupe_idx
// (index row max 2704 bytes, shared with scope and type).
const maxContentBytes = 2000

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// LoCoMoCategory is the paper's name for a category number.
func LoCoMoCategory(n string) string {
	switch n {
	case "1":
		return "1 single-hop"
	case "2":
		return "2 multi-hop"
	case "3":
		return "3 temporal"
	case "4":
		return "4 open-domain"
	default:
		return n
	}
}

func pct(n, d int) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", float64(n)/float64(d))
}

func meanStr(sum float64, n int) string {
	if n == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", sum/float64(n))
}
