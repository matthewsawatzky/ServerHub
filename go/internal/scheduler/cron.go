package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronSpec is a parsed 5-field cron expression (minute hour day-of-month month
// day-of-week). Numeric fields only; day-of-week accepts 0 or 7 for Sunday.
type cronSpec struct {
	minutes map[int]bool
	hours   map[int]bool
	dom     map[int]bool
	months  map[int]bool
	dow     map[int]bool
	domStar bool
	dowStar bool
}

func parseCron(expr string) (*cronSpec, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron must have 5 fields (minute hour day month weekday)")
	}
	c := &cronSpec{}
	var err error
	if c.minutes, err = parseCronField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("cron minute: %v", err)
	}
	if c.hours, err = parseCronField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("cron hour: %v", err)
	}
	if c.dom, err = parseCronField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("cron day of month: %v", err)
	}
	if c.months, err = parseCronField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("cron month: %v", err)
	}
	dow, err := parseCronField(fields[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("cron weekday: %v", err)
	}
	if dow[7] {
		dow[0] = true
		delete(dow, 7)
	}
	c.dow = dow
	c.domStar = strings.TrimSpace(fields[2]) == "*"
	c.dowStar = strings.TrimSpace(fields[4]) == "*"
	if len(c.minutes) == 0 || len(c.hours) == 0 || len(c.dom) == 0 || len(c.months) == 0 || len(c.dow) == 0 {
		return nil, fmt.Errorf("cron field has no values")
	}
	return c, nil
}

func parseCronField(field string, min, max int) (map[int]bool, error) {
	out := make(map[int]bool)
	for _, part := range strings.Split(strings.TrimSpace(field), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty value")
		}
		step := 1
		base := part
		if i := strings.Index(part, "/"); i >= 0 {
			base = part[:i]
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid step %q", part[i+1:])
			}
			step = n
		}
		lo, hi := min, max
		switch {
		case base == "*":
			// full range
		case strings.Contains(base, "-"):
			bits := strings.SplitN(base, "-", 2)
			a, err1 := strconv.Atoi(strings.TrimSpace(bits[0]))
			b, err2 := strconv.Atoi(strings.TrimSpace(bits[1]))
			if err1 != nil || err2 != nil || a < min || b > max || a > b {
				return nil, fmt.Errorf("invalid range %q", base)
			}
			lo, hi = a, b
		default:
			a, err := strconv.Atoi(base)
			if err != nil || a < min || a > max {
				return nil, fmt.Errorf("invalid value %q", base)
			}
			lo, hi = a, a
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	return out, nil
}

func (c *cronSpec) dayMatches(t time.Time) bool {
	domOK := c.dom[t.Day()]
	dowOK := c.dow[int(t.Weekday())]
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default:
		// Standard cron semantics: either day field may match.
		return domOK || dowOK
	}
}

func (c *cronSpec) next(after time.Time) time.Time {
	loc := after.Location()
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(5, 0, 0)
	baseLabel := cronWallLabel(after)
	for !t.After(limit) {
		if !c.months[int(t.Month())] {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !c.hours[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if !c.minutes[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		// A fall-back transition repeats an hour's wall-clock labels; without
		// this check a "30 1 * * *" schedule would fire at both 01:30s on the
		// transition day. A candidate is only eligible if its local label is
		// strictly after the base's (monotonic for every scan that started at
		// a real previous run).
		if cronWallLabel(t) <= baseLabel {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// cronWallLabel packs a local wall-clock minute into one comparable number:
// YYYYMMDDHHMM.
func cronWallLabel(t time.Time) int64 {
	label := int64(t.Year())*100000000 +
		int64(int(t.Month()))*10000 +
		int64(t.Day())*100 +
		int64(t.Hour())
	return label*100 + int64(t.Minute())
}
