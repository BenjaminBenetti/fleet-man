package fleet

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A minimal standard 5-field cron implementation (minute hour day-of-month
// month day-of-week), used by Schedule automation triggers. Supporting just the
// classic syntax — `*`, single values, `a-b` ranges, comma lists, and `*/s` /
// `a-b/s` steps — keeps the dependency surface at zero (no third-party cron
// library) while covering the expressions a user realistically types into a
// schedule trigger. Evaluation is minute-granular: a schedule fires at most once
// per matching wall-clock minute.

// cronField is one parsed cron field: the set of integer values it matches. A
// `*` field matches every value in the field's range and is tracked separately
// so the day-of-month / day-of-week "or" rule (below) can tell a restricted
// field from an unrestricted one.
type cronField struct {
	star bool
	vals map[int]bool
}

func (f cronField) matches(v int) bool {
	if f.star {
		return true
	}
	return f.vals[v]
}

// CronSchedule is a parsed cron expression ready for repeated matching.
type CronSchedule struct {
	minute cronField
	hour   cronField
	dom    cronField
	month  cronField
	dow    cronField
}

// ParseCron parses a standard 5-field cron expression. Fields are separated by
// runs of whitespace; an expression with any other field count is rejected.
func ParseCron(spec string) (CronSchedule, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return CronSchedule{}, fmt.Errorf("cron expression must have 5 fields (got %d): %q", len(fields), spec)
	}
	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("minute field: %w", err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("hour field: %w", err)
	}
	dom, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("day-of-month field: %w", err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("month field: %w", err)
	}
	// Day-of-week accepts 0-7 (both 0 and 7 are Sunday); parseCronField folds 7
	// into 0 after expansion so a range like 5-7 (Fri-Sun) parses correctly.
	dow, err := parseCronField(fields[4], 0, 7)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("day-of-week field: %w", err)
	}
	return CronSchedule{minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

// Matches reports whether t falls in a minute the schedule fires on. It follows
// the classic Vixie-cron day rule: when BOTH day-of-month and day-of-week are
// restricted (neither is `*`), the day matches if EITHER field matches;
// otherwise both must match (a `*` field always matches).
func (s CronSchedule) Matches(t time.Time) bool {
	if !s.minute.matches(t.Minute()) || !s.hour.matches(t.Hour()) || !s.month.matches(int(t.Month())) {
		return false
	}
	domMatch := s.dom.matches(t.Day())
	dowMatch := s.dow.matches(int(t.Weekday()))
	if !s.dom.star && !s.dow.star {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}

// ValidateCron returns an error if spec is not a parseable cron expression.
func ValidateCron(spec string) error {
	_, err := ParseCron(spec)
	return err
}

// parseCronField parses one comma-separated cron field into the set of integers
// it matches within [min, max]. day-of-week additionally accepts 7 as Sunday
// (mapped to 0), matching common cron implementations.
func parseCronField(field string, min, max int) (cronField, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return cronField{}, fmt.Errorf("empty field")
	}
	if field == "*" {
		return cronField{star: true}, nil
	}
	vals := make(map[int]bool)
	for _, part := range strings.Split(field, ",") {
		if err := parseCronPart(strings.TrimSpace(part), min, max, vals); err != nil {
			return cronField{}, err
		}
	}
	if len(vals) == 0 {
		return cronField{}, fmt.Errorf("no values in field %q", field)
	}
	// Day-of-week (the only field parsed with max 7) treats 7 as Sunday: fold it
	// into 0 AFTER range/list expansion, so "5-7" yields {5,6,0} rather than
	// being rejected as a descending "5-0" range.
	if max == 7 {
		if vals[7] {
			vals[0] = true
		}
		delete(vals, 7)
	}
	return cronField{vals: vals}, nil
}

// parseCronPart parses a single comma-separated component — `*`, `a`, `a-b`,
// `*/s`, `a/s`, or `a-b/s` — adding every matched value to vals.
func parseCronPart(part string, min, max int, vals map[int]bool) error {
	if part == "" {
		return fmt.Errorf("empty list element")
	}
	rangeSpec := part
	step := 1
	if slash := strings.IndexByte(part, '/'); slash >= 0 {
		rangeSpec = part[:slash]
		stepStr := part[slash+1:]
		s, err := strconv.Atoi(stepStr)
		if err != nil || s <= 0 {
			return fmt.Errorf("invalid step %q", stepStr)
		}
		step = s
	}

	low, high := min, max
	switch {
	case rangeSpec == "*":
		// full range
	case strings.IndexByte(rangeSpec, '-') >= 0:
		bounds := strings.SplitN(rangeSpec, "-", 2)
		l, err := cronAtoi(bounds[0], min, max)
		if err != nil {
			return err
		}
		h, err := cronAtoi(bounds[1], min, max)
		if err != nil {
			return err
		}
		if l > h {
			return fmt.Errorf("range %q is descending", rangeSpec)
		}
		low, high = l, h
	default:
		v, err := cronAtoi(rangeSpec, min, max)
		if err != nil {
			return err
		}
		// A bare value with no slash matches exactly that value; with a slash
		// (`a/s`) it means "from a to the field max, stepping by s".
		low, high = v, v
		if step != 1 {
			high = max
		}
	}

	for v := low; v <= high; v += step {
		vals[v] = true
	}
	return nil
}

// cronAtoi parses a single field value and enforces the field's [min, max]
// bounds. Day-of-week's max is 7 (Sunday written as either 0 or 7); the 7→0
// fold happens in parseCronField after expansion, not here, so ranges ending in
// 7 keep their ascending order.
func cronAtoi(s string, min, max int) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", s)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("value %d out of range [%d-%d]", v, min, max)
	}
	return v, nil
}
