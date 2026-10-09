package bench

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	loCoMoMemoryHeader = regexp.MustCompile(`(?s)^\[([^\]\n]+), session ([0-9]+), D([0-9]+):([0-9]+)\] [^:\n]+: (.*)$`)
	calendarExpression = regexp.MustCompile(`(?i)\b(yesterday|tomorrow|([0-9]{1,5}|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve) (days?|weeks?) ago|(last|next) (week|month|year)|last (Sunday|Monday|Tuesday|Wednesday|Thursday|Friday|Saturday))\b`)
	calendarModifier   = regexp.MustCompile(`(?i)^(about|around|roughly|approximately|almost|nearly|over|under|more|less|than|least|most|exactly|maybe|not|between|either|or|to|and|[0-9]+|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundred|thousand)$`)
)

// Calendar hints interpret expressions, not events: no statement about who did
// what is created. The original memories remain the only source of those facts.
func calendarHints(contents []string) string {
	var hints strings.Builder
	for i, content := range contents {
		// Match the exact text the reader sees, not hidden/clipped source tails.
		visible := clip(strings.TrimSpace(content), 2000)
		parts := loCoMoMemoryHeader.FindStringSubmatch(visible)
		if parts == nil || parts[2] != parts[3] {
			continue
		}
		anchor, err := parseLoCoMoDate(parts[1])
		if err != nil {
			continue
		}
		anchor = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC)
		utterance, _, _ := strings.Cut(parts[5], "[image:")
		seen := map[string]bool{}
		for _, positions := range calendarExpression.FindAllStringSubmatchIndex(utterance, 16) {
			if !completeCalendarExpression(utterance, positions[0], positions[1], len(strings.TrimSpace(content)) >= 2000 && len(utterance) == len(parts[5])) {
				continue
			}
			match := make([]string, len(positions)/2)
			for j := range match {
				if positions[2*j] >= 0 {
					match[j] = utterance[positions[2*j]:positions[2*j+1]]
				}
			}
			expression := strings.ToLower(match[0])
			if seen[expression] {
				continue
			}
			seen[expression] = true
			value := calendarMeaning(anchor, match)
			if value == "" {
				continue
			}
			line := fmt.Sprintf("Memory %d (D%s:%s), calendar date %s, %q: %s\n", i+1, parts[3], parts[4], anchor.Format("2006-01-02"), match[0], value)
			if hints.Len()+len(line) > 8000 {
				return hints.String()
			}
			hints.WriteString(line)
		}
	}
	return hints.String()
}

// Reject unsupported composites/qualifiers instead of guessing their precision.
func completeCalendarExpression(text string, start, end int, clipped bool) bool {
	if clipped && end == len(text) {
		return false
	}
	before, _ := utf8.DecodeLastRuneInString(text[:start])
	after, _ := utf8.DecodeRuneInString(text[end:])
	for _, r := range []rune{before, after} {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("_-+/–—", r) {
			return false
		}
	}
	prefix := strings.TrimSpace(text[:start])
	last, _ := utf8.DecodeLastRuneInString(prefix)
	if strings.ContainsRune(".,/-+–—", last) {
		// A comma/period touching a numeric match can be part of its count.
		if text[start] >= '0' && text[start] <= '9' || strings.ContainsRune("/-+–—", last) {
			return false
		}
	}
	words := strings.Fields(prefix)
	if len(words) > 0 && calendarModifier.MatchString(words[len(words)-1]) {
		return false
	}
	suffix := strings.Fields(strings.TrimLeft(text[end:], " ,;"))
	return len(suffix) == 0 || !calendarModifier.MatchString(strings.Trim(suffix[0], ".,!?:;"))
}

func calendarMeaning(anchor time.Time, match []string) string {
	date := func(d time.Time) string {
		if d.Year() < 1 || d.Year() > 9999 {
			return ""
		}
		return d.Format("2006-01-02")
	}
	expression := strings.ToLower(match[0])
	if expression == "yesterday" {
		return date(anchor.AddDate(0, 0, -1))
	}
	if expression == "tomorrow" {
		return date(anchor.AddDate(0, 0, 1))
	}
	if match[2] != "" {
		n, err := strconv.Atoi(match[2])
		if err != nil {
			n = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12}[strings.ToLower(match[2])]
		}
		if n < 1 || n > 3660 {
			return ""
		}
		weeks := strings.Contains(expression, "week")
		if weeks {
			n *= 7
		}
		value := date(anchor.AddDate(0, 0, -n))
		if weeks && value != "" {
			value = "approximately " + value
		}
		return value
	}
	if match[6] != "" {
		for weekday := time.Sunday; weekday <= time.Saturday; weekday++ {
			if !strings.EqualFold(weekday.String(), match[6]) {
				continue
			}
			days := (int(anchor.Weekday()) - int(weekday) + 7) % 7
			if days == 0 {
				days = 7
			}
			if value := date(anchor.AddDate(0, 0, -days)); value != "" {
				return value + " (strictly preceding weekday convention; original context decides)"
			}
			return ""
		}
	}
	direction := -1
	if strings.EqualFold(match[4], "next") {
		direction = 1
	}
	var first, last time.Time
	suffix := ""
	switch strings.ToLower(match[5]) {
	case "week":
		monday := anchor.AddDate(0, 0, -(int(anchor.Weekday())+6)%7)
		first = monday.AddDate(0, 0, 7*direction)
		last = first.AddDate(0, 0, 6)
		suffix = " (calendar week, Monday-Sunday; exact day unspecified)"
	case "month":
		first = time.Date(anchor.Year(), anchor.Month()+time.Month(direction), 1, 0, 0, 0, 0, time.UTC)
		last = first.AddDate(0, 1, -1)
	case "year":
		first = time.Date(anchor.Year()+direction, 1, 1, 0, 0, 0, 0, time.UTC)
		last = time.Date(anchor.Year()+direction, 12, 31, 0, 0, 0, 0, time.UTC)
	default:
		return ""
	}
	if date(first) == "" || date(last) == "" {
		return ""
	}
	return date(first) + ".." + date(last) + suffix
}
