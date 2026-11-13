// Package limits provides the admission-control layer the pipeline uses in
// front of the model provider. It reserves future per-minute windows so
// concurrent requests queue at different slots rather than all retrying
// together at the same boundary.
package limits

import (
	"context"
	"sync"
	"time"
)

type windowUsage struct {
	tokens   int
	requests int
}

// Bucket is an admission gate over fixed one-minute service windows plus a
// rolling local day. It deliberately mirrors the stand-in's simpler windows;
// the real provider can still reject a call, in which case retry-after wins.
type Bucket struct {
	mu sync.Mutex

	TPM int
	RPM int
	TPD int

	windowStart time.Time
	dayStart    time.Time
	tokToday    int
	windows     map[time.Time]*windowUsage
	disabled    bool
}

// New builds a bucket. A non-positive value disables that limit. Windows are
// aligned to wall-clock minutes so a client mirroring the service rolls at the
// same instant it does; an unaligned start made boundary calls look admissible
// when the service had already begun a new window.
func New(tpm, rpm, tpd int) *Bucket {
	now := time.Now()
	return &Bucket{TPM: tpm, RPM: rpm, TPD: tpd, windowStart: now.Truncate(time.Minute), dayStart: now,
		windows: map[time.Time]*windowUsage{}}
}

// Disabled returns a bucket that never blocks.
func Disabled() *Bucket { return &Bucket{disabled: true} }

// Decision is the outcome of an admission check.
type Decision struct {
	Allow        bool
	Wait         time.Duration
	RetryAfter   time.Duration
	Reason       string // "ok", "per-minute", "per-day", "too-large"
	DayExhausted bool
	// TooLarge means the call cannot fit in any single window, so waiting will
	// never help. Without this guard Admit would scan windows forever.
	TooLarge bool

	reservedWindow time.Time
}

// Admit reserves capacity and returns how long the caller must wait before
// using it. Reservations are essential: merely returning the same wait to all
// callers creates a thundering herd when that wait expires.
func (b *Bucket) Admit(estTokens int) Decision {
	if b.disabled {
		return Decision{Allow: true, Reason: "ok"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.dayStart) >= 24*time.Hour {
		b.dayStart = now
		b.tokToday = 0
	}
	if b.TPD > 0 && b.tokToday+estTokens > b.TPD {
		return Decision{Allow: false, RetryAfter: time.Until(b.dayStart.Add(24 * time.Hour)),
			Reason: "per-day", DayExhausted: true}
	}
	// A call larger than a whole window can never be admitted by waiting.
	// Scanning forward would loop forever, so say so explicitly.
	if (b.TPM > 0 && estTokens > b.TPM) || (b.RPM > 0 && b.RPM < 1) {
		return Decision{Allow: false, Reason: "too-large", TooLarge: true}
	}

	candidate := b.windowStart
	if now.Sub(candidate) >= time.Minute {
		candidate = now.Truncate(time.Minute)
		b.windowStart = candidate
	}
	for {
		u := b.windows[candidate]
		if u == nil {
			u = &windowUsage{}
			b.windows[candidate] = u
		}
		tokensFit := b.TPM <= 0 || u.tokens+estTokens <= b.TPM
		requestsFit := b.RPM <= 0 || u.requests+1 <= b.RPM
		if tokensFit && requestsFit {
			u.tokens += estTokens
			u.requests++
			b.tokToday += estTokens
			wait := candidate.Sub(now)
			if wait < 0 {
				wait = 0
			}
			reason := "ok"
			if wait > 0 {
				reason = "per-minute"
			}
			return Decision{Allow: true, Wait: wait, Reason: reason, reservedWindow: candidate}
		}
		candidate = candidate.Add(time.Minute)
	}
}

// Observe corrects the reservation that authorized a call with actual usage.
// The correction belongs to the reserved window, not whichever window happens
// to be current when a delayed response arrives.
func (b *Bucket) Observe(d Decision, estTokens, actualTokens int) {
	if b.disabled || !d.Allow || d.reservedWindow.IsZero() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delta := actualTokens - estTokens
	if u := b.windows[d.reservedWindow]; u != nil {
		u.tokens += delta
	}
	b.tokToday += delta
}

// Snapshot reports current-window and daily usage for logging.
func (b *Bucket) Snapshot() (tokThisMin, tokToday int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u := b.windows[b.windowStart]; u != nil {
		tokThisMin = u.tokens
	}
	return tokThisMin, b.tokToday
}

// Wait observes a decision, sleeping if needed. Returns false if the context
// ended first.
func Wait(ctx context.Context, d Decision) bool {
	if d.Wait <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d.Wait):
		return true
	}
}
