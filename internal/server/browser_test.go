package server

import "testing"

// TestShouldUseLandingPage covers the precedence between a configured
// browser.initialUrl and the Fleet Launch landing page, gated by the fleet's
// PreferFleetLaunch setting. (Moved from the TUI when the browser's container
// logic moved server-side.)
func TestShouldUseLandingPage(t *testing.T) {
	cases := []struct {
		name              string
		hasURL            bool
		hasLanding        bool
		preferFleetLaunch bool
		want              bool
	}{
		{"neither configured", false, false, false, false},
		{"neither, prefer on", false, false, true, false},
		{"only url", true, false, false, false},
		{"only url, prefer on", true, false, true, false},
		{"only landing", false, true, false, true},
		{"only landing, prefer on", false, true, true, true},
		{"both, prefer off -> url wins", true, true, false, false},
		{"both, prefer on -> landing wins", true, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldUseLandingPage(tc.hasURL, tc.hasLanding, tc.preferFleetLaunch); got != tc.want {
				t.Errorf("shouldUseLandingPage(url=%v, landing=%v, prefer=%v) = %v, want %v",
					tc.hasURL, tc.hasLanding, tc.preferFleetLaunch, got, tc.want)
			}
		})
	}
}
