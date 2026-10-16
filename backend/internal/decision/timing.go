package decision

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"personal-crm/backend/internal/cadence"
)

// A timing expression found in a message, and the calendar date it resolves
// to against the message's own timestamp, never the time a run reads it.
type found struct {
	expression string
	date       time.Time
}

const monthPattern = `(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)`

// timingRules are the date and timing expressions the search knows: weekday
// names, where "this" names the next such day, the sent date included, and
// "next" that day in the following week; today, tonight and tomorrow; this or
// next week and weekend; month and day; numeric calendar dates; "the 17th";
// "in N days" or weeks. Each resolves against the local calendar date the
// message was sent on.
var timingRules = []struct {
	pattern *regexp.Regexp
	resolve func(m []string, sent time.Time) (time.Time, bool)
}{
	{regexp.MustCompile(`(?i)\b(?:(next|this)\s+)?(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`), func(m []string, sent time.Time) (time.Time, bool) {
		weekday := weekdays[strings.ToLower(m[2])]
		ahead := (weekday - int(sent.Weekday()) + 7) % 7 // to the next such day, today included
		switch {
		case strings.EqualFold(m[1], "next"): // that day in the week after the sent date's
			return nextMonday(sent).AddDate(0, 0, (weekday+6)%7), true
		case strings.EqualFold(m[1], "this"), ahead > 0:
			return sent.AddDate(0, 0, ahead), true
		default: // a bare weekday names the next such day after the sent date
			return sent.AddDate(0, 0, 7), true
		}
	}},
	{regexp.MustCompile(`(?i)\b(today|tonight|tomorrow)\b`), func(m []string, sent time.Time) (time.Time, bool) {
		if strings.EqualFold(m[1], "tomorrow") {
			return sent.AddDate(0, 0, 1), true
		}
		return sent, true
	}},
	{regexp.MustCompile(`(?i)\b(this|next)\s+(week|weekend)\b`), func(m []string, sent time.Time) (time.Time, bool) {
		monday := sinceMonday(sent)
		next := strings.EqualFold(m[1], "next")
		switch {
		case strings.EqualFold(m[2], "week") && next: // Monday of next week
			return nextMonday(sent), true
		case strings.EqualFold(m[2], "week"): // this week's Friday, or today once past it
			return sent.AddDate(0, 0, max(4-monday, 0)), true
		case next: // next week's Saturday
			return nextMonday(sent).AddDate(0, 0, 5), true
		default: // this weekend's Saturday, or today on its Sunday
			return sent.AddDate(0, 0, max(5-monday, 0)), true
		}
	}},
	{regexp.MustCompile(`(?i)\b` + monthPattern + `\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b(?:,?\s+(\d{4})\b)?`), func(m []string, sent time.Time) (time.Time, bool) {
		return monthDay(sent, months[strings.ToLower(m[1])[:3]], m[2], m[3])
	}},
	{regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(?:of\s+)?` + monthPattern + `\b(?:,?\s+(\d{4})\b)?`), func(m []string, sent time.Time) (time.Time, bool) {
		return monthDay(sent, months[strings.ToLower(m[2])[:3]], m[1], m[3])
	}},
	{regexp.MustCompile(`\b(\d{4})-(\d{1,2})-(\d{1,2})\b`), func(m []string, _ time.Time) (time.Time, bool) {
		return date(atoi(m[1]), atoi(m[2]), atoi(m[3]))
	}},
	{regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})(?:/(\d{4}|\d{2}))?\b`), func(m []string, sent time.Time) (time.Time, bool) {
		year := m[3]
		if len(year) == 2 {
			year = "20" + year
		}
		return monthDay(sent, atoi(m[1]), m[2], year)
	}},
	{regexp.MustCompile(`(?i)\bthe\s+(\d{1,2})(?:st|nd|rd|th)\b`), func(m []string, sent time.Time) (time.Time, bool) {
		day := atoi(m[1])
		for month := 0; month < 12; month++ { // the next month with that day
			first := time.Date(sent.Year(), sent.Month()+time.Month(month), 1, 0, 0, 0, 0, time.UTC)
			if d, ok := date(first.Year(), int(first.Month()), day); ok && !d.Before(sent) {
				return d, true
			}
		}
		return time.Time{}, false
	}},
	{regexp.MustCompile(`(?i)\bin\s+(\d{1,3}|a|one|two|three|four|five|six|seven|eight|nine|ten)\s+(days?|weeks?)\b`), func(m []string, sent time.Time) (time.Time, bool) {
		n, ok := numberWords[strings.ToLower(m[1])]
		if !ok {
			n = atoi(m[1])
		}
		if strings.HasPrefix(strings.ToLower(m[2]), "week") {
			n *= 7
		}
		return sent.AddDate(0, 0, n), true
	}},
}

// Weeks start on Monday: sinceMonday counts the days since the sent date's
// Monday, and nextMonday is the Monday of the following week.
func sinceMonday(sent time.Time) int { return (int(sent.Weekday()) + 6) % 7 }

func nextMonday(sent time.Time) time.Time { return sent.AddDate(0, 0, 7-sinceMonday(sent)) }

var weekdays = map[string]int{"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6}

var months = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}

var numberWords = map[string]int{"a": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10}

// findDates returns the timing expressions in text with the dates they
// resolve to, in text order.
func findDates(text string, sentAt time.Time) []found {
	sent := cadence.CalendarDate(cadence.DateOnly(sentAt))
	type match struct {
		start int
		found
	}
	var matches []match
	for _, rule := range timingRules {
		for _, idx := range rule.pattern.FindAllStringSubmatchIndex(text, -1) {
			groups := make([]string, len(idx)/2)
			for g := range groups {
				if idx[2*g] >= 0 {
					groups[g] = text[idx[2*g]:idx[2*g+1]]
				}
			}
			if d, ok := rule.resolve(groups, sent); ok {
				matches = append(matches, match{idx[0], found{groups[0], d}})
			}
		}
	}
	slices.SortStableFunc(matches, func(a, b match) int { return a.start - b.start })
	out := make([]found, len(matches))
	for i, m := range matches {
		out[i] = m.found
	}
	return out
}

// monthDay resolves a month and day, in the given year or else the next
// occurrence on or after the sent date.
func monthDay(sent time.Time, month int, day, year string) (time.Time, bool) {
	if year != "" {
		return date(atoi(year), month, atoi(day))
	}
	d, ok := date(sent.Year(), month, atoi(day))
	if ok && d.Before(sent) {
		d, ok = date(sent.Year()+1, month, atoi(day))
	}
	return d, ok
}

// date returns a calendar date, and false when it does not exist.
func date(year, month, day int) (time.Time, bool) {
	d := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return d, d.Year() == year && int(d.Month()) == month && d.Day() == day
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
