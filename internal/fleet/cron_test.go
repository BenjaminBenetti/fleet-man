package fleet

import (
	"testing"
	"time"
)

func TestParseCronInvalid(t *testing.T) {
	cases := []string{
		"",            // empty
		"* * * *",     // 4 fields
		"* * * * * *", // 6 fields
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // day-of-month out of range
		"* * * 13 *",  // month out of range
		"* * * * 8",   // day-of-week out of range
		"*/0 * * * *", // zero step
		"5-1 * * * *", // descending range
		"a * * * *",   // non-numeric
		"* * * * 1-",  // empty range bound
	}
	for _, c := range cases {
		if err := ValidateCron(c); err == nil {
			t.Errorf("ValidateCron(%q) = nil, want error", c)
		}
	}
}

func TestParseCronValid(t *testing.T) {
	cases := []string{
		"* * * * *",
		"0 0 * * *",
		"*/15 * * * *",
		"0 9-17 * * 1-5",
		"0,30 * * * *",
		"5 4 1 1 *",
		"0 0 * * 7", // Sunday as 7
		"0-59/10 * * * *",
	}
	for _, c := range cases {
		if err := ValidateCron(c); err != nil {
			t.Errorf("ValidateCron(%q) = %v, want nil", c, err)
		}
	}
}

func TestCronMatches(t *testing.T) {
	// 2026-06-20 is a Saturday (weekday 6); 2026-06-22 is a Monday (weekday 1).
	mustMatch := func(spec string, ts time.Time, want bool) {
		t.Helper()
		s, err := ParseCron(spec)
		if err != nil {
			t.Fatalf("ParseCron(%q): %v", spec, err)
		}
		if got := s.Matches(ts); got != want {
			t.Errorf("ParseCron(%q).Matches(%s) = %v, want %v", spec, ts.Format(time.RFC3339), got, want)
		}
	}

	sat := time.Date(2026, 6, 20, 9, 30, 0, 0, time.UTC) // Saturday 09:30
	mon := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)  // Monday 09:00

	mustMatch("* * * * *", sat, true)
	mustMatch("30 9 * * *", sat, true)
	mustMatch("31 9 * * *", sat, false)
	mustMatch("*/15 * * * *", sat, true)  // 30 is divisible by 15
	mustMatch("*/20 * * * *", sat, false) // 30 not divisible by 20
	mustMatch("0 9-17 * * 1-5", mon, true)
	mustMatch("0 9-17 * * 1-5", sat, false) // Saturday excluded by dow
	mustMatch("0 9 22 6 *", mon, true)      // day-of-month 22
	mustMatch("0,30 * * * *", sat, true)
	mustMatch("0 * * * *", sat, false)

	// Vixie day rule: when BOTH dom and dow are restricted, match if EITHER.
	mustMatch("0 9 20 * 1", mon, true)   // dow=Monday matches even though dom=20 != 22
	mustMatch("30 9 20 * 1", sat, true)  // dom=20 matches even though dow != Monday
	mustMatch("30 9 19 * 1", sat, false) // neither dom (19) nor dow (Monday) matches Saturday the 20th
}
