package limits

import (
	"testing"
	"time"
)

func TestAdmitReservesDistinctFutureWindows(t *testing.T) {
	b := New(100, 1, 1000)
	d1 := b.Admit(100)
	d2 := b.Admit(100)
	d3 := b.Admit(100)
	if !d1.Allow || d1.Wait != 0 {
		t.Fatalf("first reservation should be immediate: %+v", d1)
	}
	if !d2.Allow || d2.Wait <= 0 {
		t.Fatalf("second reservation should use a future window: %+v", d2)
	}
	if !d3.Allow || d3.Wait <= d2.Wait || d3.Wait > 3*time.Minute {
		t.Fatalf("third call did not receive a distinct slot: d2=%+v d3=%+v", d2, d3)
	}
	if d2.reservedWindow.Equal(d3.reservedWindow) {
		t.Fatal("concurrent calls would still wake in the same window")
	}
}

func TestObserveCorrectsReservedWindowAndDailyUsage(t *testing.T) {
	b := New(200, 10, 120)
	d1 := b.Admit(100)
	b.Observe(d1, 100, 80)
	if d2 := b.Admit(30); !d2.Allow || d2.Wait != 0 {
		t.Fatalf("actual headroom and daily budget were not corrected: %+v", d2)
	}
	if d3 := b.Admit(30); d3.Allow || !d3.DayExhausted {
		t.Fatalf("daily limit should reject work that cannot fit: %+v", d3)
	}
}

func TestCancelledReservationCanBeReleased(t *testing.T) {
	b := New(100, 1, 1000)
	_ = b.Admit(100)
	d := b.Admit(100)
	b.Observe(d, 100, 0) // context ended before the provider call
	_, today := b.Snapshot()
	if today != 100 {
		t.Fatalf("unused reservation was not released: daily tokens=%d", today)
	}
}

func TestAdmitRejectsCallLargerThanAWindowInsteadOfScanningForever(t *testing.T) {
	b := New(100, 10, 0)
	done := make(chan Decision, 1)
	go func() { done <- b.Admit(500) }()
	select {
	case d := <-done:
		if d.Allow || !d.TooLarge {
			t.Fatalf("oversized call should be rejected as too large: %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("Admit scanned windows forever for a call larger than the window")
	}
}
