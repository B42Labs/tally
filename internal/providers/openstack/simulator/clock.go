package simulator

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// Clock maps wall time to the virtual time of the simulated month, so that a
// month of notifications is emitted in the minutes an operator is willing to
// wait for it. Everything the simulator paces reads its time here rather than
// from time.Now, which is what lets the factor be raised or lowered while a run
// is in flight.
//
// The factor is a float64 because it scales durations and never touches a usage
// quantity or a price. The forbidigo rules in .golangci.yml that keep floats
// off the money paths are about those quantities, not about how fast the
// simulation runs.
type Clock struct {
	mu       sync.Mutex
	base     time.Time     // virtual instant at baseWall
	baseWall time.Time     // wall instant the base was taken at
	factor   float64       // virtual seconds per wall second; 0 is unbounded
	changed  chan struct{} // closed and replaced on every SetFactor
	now      func() time.Time
}

// NewClock starts a clock at start, running at factor virtual seconds per wall
// second. now is the wall clock it reads: production passes time.Now, and a
// test passes a function it advances by hand so that a month-long factor is
// asserted without waiting for real time.
func NewClock(start time.Time, factor float64, now func() time.Time) *Clock {
	return &Clock{
		base:     start,
		baseWall: now(),
		factor:   factor,
		changed:  make(chan struct{}),
		now:      now,
	}
}

// Now is the virtual instant the clock currently stands at.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.nowLocked()
}

// nowLocked is Now with the lock already held, which is what SetFactor needs to
// read the old factor's virtual now and rebase on it in one critical section.
// At factor 0 the elapsed wall time scales to nothing, so the virtual clock
// stands still however long the process runs.
func (c *Clock) nowLocked() time.Time {
	elapsed := c.now().Sub(c.baseWall)
	return c.base.Add(time.Duration(float64(elapsed) * c.factor))
}

// Factor is the virtual seconds the clock currently covers per wall second.
func (c *Clock) Factor() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.factor
}

// SetFactor changes how fast virtual time runs from this instant on. The clock
// rebases on the virtual now it has reached under the old factor, so a change
// never moves the simulated month itself: only what comes after it runs at a
// different speed. Closing changed wakes every SleepUntil, because the wall
// deadline each of them computed no longer answers to the new factor.
//
// A negative factor is refused rather than clamped: it would run the simulated
// month backwards, and every consumer of the notifications reads their
// timestamps as moving forward. NaN is refused beside it, because it compares
// false against every bound, including this one: it would reach the arithmetic
// below and turn every virtual instant the clock reports into a duration
// nothing can wait for.
func (c *Clock) SetFactor(factor float64) error {
	if math.IsNaN(factor) || factor < 0 {
		return fmt.Errorf("factor %g must be zero or positive", factor)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.base = c.nowLocked()
	c.baseWall = c.now()
	c.factor = factor
	close(c.changed)
	c.changed = make(chan struct{})
	return nil
}

// snapshot reads the three values a wait is planned from in one locked read. A
// factor change landing between them would otherwise leave the waiter holding a
// deadline computed under one factor and a changed channel that has already
// been closed for it, so the wake never arrives.
func (c *Clock) snapshot() (now time.Time, factor float64, changed <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.nowLocked(), c.factor, c.changed
}

// maxWait bounds one wall wait. A very small factor makes the exact wait longer
// than a time.Duration holds, and converting an out-of-range float64 to an
// integer is implementation-defined: it yields either a non-positive duration,
// which fires at once and turns the wait into a spin, or a saturated one, which
// never fires at all. Waking early costs nothing, because the loop recomputes
// what is left of the distance.
const maxWait = time.Hour

// wallWait is the wall time the remaining virtual distance costs at factor,
// bounded by maxWait. The factor is above zero here: SleepUntil returns before
// it reaches this at factor 0, and SetFactor refuses everything below it.
func wallWait(remaining time.Duration, factor float64) time.Duration {
	if exact := float64(remaining) / factor; exact < float64(maxWait) {
		return time.Duration(exact)
	}
	return maxWait
}

// SleepUntil waits until the virtual clock reaches virtual, and reports ctx's
// error when the context ends first. At factor 0 it returns at once: virtual
// time does not advance then, so waiting for it would never end, and the run
// goes as fast as the broker takes the notifications.
//
// The wait is recomputed on every wake because the factor decides what the
// remaining virtual distance costs in wall time. A change mid-wait invalidates
// the deadline, which is why the timer and the changed channel are selected on
// together. A distance that costs more wall time than maxWait is covered in
// several waits for the same reason.
func (c *Clock) SleepUntil(ctx context.Context, virtual time.Time) error {
	for {
		now, factor, changed := c.snapshot()
		if factor == 0 || !now.Before(virtual) {
			return nil
		}

		timer := time.NewTimer(wallWait(virtual.Sub(now), factor))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		case <-changed:
			timer.Stop()
		}
	}
}
