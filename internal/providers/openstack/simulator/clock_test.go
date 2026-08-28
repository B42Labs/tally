package simulator

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// clockStart is the first instant of the simulated month the clock tests run
// over, and 744 is the factor that covers a 31 day month in one hour.
var clockStart = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

// wake is how long a test waits for a sleeper before calling it hung. It is far
// above the wake it asserts, so the bound fails a clock that never returns
// rather than a machine that is busy.
const wake = time.Second

// awaited returns what the sleeper reported, or fails the test when it is still
// waiting after wake.
func awaited(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(wake):
		t.Fatal("SleepUntil() has not returned, want it woken")
		return nil
	}
}

func TestClockAdvancesAtTheFactor(t *testing.T) {
	wall := time.Unix(0, 0)
	clock := NewClock(clockStart, 744, func() time.Time { return wall })

	wall = wall.Add(time.Second)

	want := clockStart.Add(744 * time.Second)
	if got := clock.Now(); !got.Equal(want) {
		t.Errorf("Now() = %s, want %s", got, want)
	}
}

func TestSetFactorRebases(t *testing.T) {
	wall := time.Unix(0, 0)
	clock := NewClock(clockStart, 744, func() time.Time { return wall })

	wall = wall.Add(time.Second)
	atChange := clockStart.Add(744 * time.Second)

	if err := clock.SetFactor(10); err != nil {
		t.Fatalf("SetFactor(10) error = %v, want nil", err)
	}
	if got := clock.Now(); !got.Equal(atChange) {
		t.Errorf("Now() at the change = %s, want %s, because a change moves nothing already reached", got, atChange)
	}
	if got := clock.Factor(); got != 10 {
		t.Errorf("Factor() = %g, want 10", got)
	}

	wall = wall.Add(time.Second)

	want := atChange.Add(10 * time.Second)
	if got := clock.Now(); !got.Equal(want) {
		t.Errorf("Now() = %s, want %s", got, want)
	}
}

func TestSetFactorRefusesANegativeFactor(t *testing.T) {
	wall := time.Unix(0, 0)
	clock := NewClock(clockStart, 744, func() time.Time { return wall })

	err := clock.SetFactor(-1)
	if err == nil {
		t.Fatal("SetFactor(-1) error = nil, want an error")
	}
	if want := "factor -1 must be zero or positive"; err.Error() != want {
		t.Errorf("SetFactor(-1) error = %q, want %q", err.Error(), want)
	}
	if got := clock.Factor(); got != 744 {
		t.Errorf("Factor() = %g, want 744, because a refused change leaves the clock alone", got)
	}
}

func TestSetFactorRefusesNaN(t *testing.T) {
	wall := time.Unix(0, 0)
	clock := NewClock(clockStart, 744, func() time.Time { return wall })

	err := clock.SetFactor(math.NaN())
	if err == nil {
		t.Fatal("SetFactor(NaN) error = nil, want an error")
	}
	if want := "factor NaN must be zero or positive"; err.Error() != want {
		t.Errorf("SetFactor(NaN) error = %q, want %q", err.Error(), want)
	}
	if got := clock.Factor(); got != 744 {
		t.Errorf("Factor() = %g, want 744, because a refused change leaves the clock alone", got)
	}
}

func TestWallWaitIsBounded(t *testing.T) {
	// A wait that is not bounded is not a slow wait: the conversion of a float
	// that a time.Duration cannot hold is implementation-defined, and the two
	// answers it gives are a wait that fires at once, which spins a core over a
	// month that publishes nothing, and one of some three hundred years.
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		factor    float64
		want      time.Duration
	}{
		{"real time at factor 1", time.Minute, 1, time.Minute},
		{"a month-in-an-hour factor", 744 * time.Second, 744, time.Second},
		{"a distance past the bound", 30 * 24 * time.Hour, 1, maxWait},
		{"a factor that overflows the conversion", time.Hour, 1e-7, maxWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wallWait(tc.remaining, tc.factor); got != tc.want {
				t.Errorf("wallWait(%s, %g) = %s, want %s", tc.remaining, tc.factor, got, tc.want)
			}
		})
	}
}

func TestSleepUntilReturnsAtOnceWhenUnbounded(t *testing.T) {
	wall := time.Unix(0, 0)
	clock := NewClock(clockStart, 0, func() time.Time { return wall })

	done := make(chan error, 1)
	go func() { done <- clock.SleepUntil(t.Context(), clockStart.Add(time.Hour)) }()

	if err := awaited(t, done); err != nil {
		t.Errorf("SleepUntil() error = %v, want nil at factor 0", err)
	}
}

func TestSleepUntilWakesOnAFactorChange(t *testing.T) {
	clock := NewClock(clockStart, 1, time.Now)

	done := make(chan error, 1)
	go func() { done <- clock.SleepUntil(t.Context(), clockStart.Add(time.Hour)) }()

	// At factor 1 the sleeper is an hour of wall time from its target, so it is
	// still waiting when the factor drops and only the change can wake it.
	time.Sleep(50 * time.Millisecond)
	if err := clock.SetFactor(0); err != nil {
		t.Fatalf("SetFactor(0) error = %v, want nil", err)
	}

	if err := awaited(t, done); err != nil {
		t.Errorf("SleepUntil() error = %v, want nil once the factor is 0", err)
	}
}

func TestSleepUntilReturnsTheContextError(t *testing.T) {
	clock := NewClock(clockStart, 1, time.Now)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- clock.SleepUntil(ctx, clockStart.Add(time.Hour)) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	if err := awaited(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("SleepUntil() error = %v, want %v", err, context.Canceled)
	}
}
