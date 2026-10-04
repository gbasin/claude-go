package repl

import "testing"

func TestParseLoop(t *testing.T) {
	cases := []struct {
		arg          string
		wantSec      int
		wantPrompt   string
		wantInterval bool
	}{
		{"5m /babysit-prs", 300, "/babysit-prs", true},
		{"2h check the deploy", 7200, "check the deploy", true},
		{"check the deploy every 20m", 1200, "check the deploy", true},
		{"run tests every 5 minutes", 300, "run tests", true},
		{"check the deploy every 2 hours", 7200, "check the deploy", true},
		{"check the deploy", 0, "check the deploy", false},
		{"check every PR", 0, "check every PR", false},
		{"300 check the build", 300, "check the build", true}, // legacy seconds
	}
	for _, c := range cases {
		sec, prompt, has := parseLoop(c.arg)
		if sec != c.wantSec || prompt != c.wantPrompt || has != c.wantInterval {
			t.Errorf("parseLoop(%q) = (%d,%q,%v) want (%d,%q,%v)",
				c.arg, sec, prompt, has, c.wantSec, c.wantPrompt, c.wantInterval)
		}
	}
}

// The cloud-offer trigger (mod_1684.js x()): parsed ≥60-min interval, or
// daily phrasing anywhere in the input.
func TestLoopCloudOfferTrigger(t *testing.T) {
	offers := map[string]bool{
		"2h check the deploy":          true, // ≥60min
		"1h status":                    true, // exactly 60min
		"every morning tell me a joke": true, // daily phrase, no interval
		"check the deploy daily":       true,
		"5m check the deploy":          false, // <60min
		"30m standup notes":            false,
		"check the deploy":             false,
		"check every PR":               false, // "every" not followed by time
		"5m check the daily report":    true,  // daily phrase in prompt text
	}
	for arg, want := range offers {
		sec, _, has := parseLoop(arg)
		got := (has && sec >= 3600) || loopDailyRe.MatchString(arg)
		if got != want {
			t.Errorf("cloudOffer(%q) = %v want %v", arg, got, want)
		}
	}
}
