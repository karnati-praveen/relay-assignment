package main

import "testing"

// A sweep that waits only for the provider's retry-after never converges: the
// retry restarts the whole request and re-spends the planner and draft tokens
// the failed attempt already paid for. These cases pin the arithmetic that
// makes one wait enough for one whole attempt.
func TestBudgetWaitSeconds(t *testing.T) {
	const tpd = 200000 // 2.3148 tokens/s

	cases := []struct {
		name       string
		spent      int
		retryAfter int
		want       int
	}{
		// Nothing spent yet: only the provider's own hint plus the margin.
		{"no spend", 0, 60, 75},
		// One draft burned (2,047 + 198) then the check was refused with 120 s
		// to go: the pause must also earn those 2,245 tokens back.
		{"draft already paid for", 2245, 120, 120 + 969 + 15},
		// The divergence case: a 150 s hint with 2,327 tokens already spent.
		{"retry-after alone is not enough", 2327, 150, 150 + 1005 + 15},
	}
	for _, c := range cases {
		got := budgetWaitSeconds(c.spent, c.retryAfter, tpd)
		if got != c.want {
			t.Errorf("%s: budgetWaitSeconds(%d, %d) = %d, want %d",
				c.name, c.spent, c.retryAfter, got, c.want)
		}
		// A wait must always fund at least the retry-after plus what was spent.
		if got < c.retryAfter {
			t.Errorf("%s: wait %d is shorter than the provider's hint %d",
				c.name, got, c.retryAfter)
		}
	}
}

// The refill rate is the provider's own: 200,000 tokens spread over 24 hours,
// which is what its "try again in 9m31.5s" implied at 199,437 used.
func TestDailyRefillPerSec(t *testing.T) {
	if got := dailyRefillPerSec(200000); got < 2.31 || got > 2.32 {
		t.Errorf("dailyRefillPerSec(200000) = %v, want ~2.315", got)
	}
	if got := dailyRefillPerSec(0); got != 0 {
		t.Errorf("dailyRefillPerSec(0) = %v, want 0", got)
	}
	// With no refill model we must not invent a wait.
	if got := budgetWaitSeconds(5000, 42, 0); got != 42 {
		t.Errorf("budgetWaitSeconds with unknown tpd = %d, want the hint 42", got)
	}
}
