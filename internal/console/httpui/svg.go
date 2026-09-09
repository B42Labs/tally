package httpui

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// chartWidth is how wide every drawing on the console is, in user units. The
// bars and the timeline share it, so a page holding both lines them up.
const chartWidth int64 = 640

// barWidth scales one value against the largest value of its table. A table
// whose largest value is zero draws no bar at all rather than dividing by it:
// money.Div panics on a zero divisor, the way the decimal package does.
func barWidth(value, max decimal.Decimal) int64 {
	if max.IsZero() || value.Sign() <= 0 {
		return 0
	}
	return money.Div(value, max).Mul(decimal.NewFromInt(chartWidth)).IntPart()
}

// lane is one rectangle of a timeline: where it starts, how wide it is, the
// class its colour comes from, and what is written on it.
type lane struct {
	X     int64
	W     int64
	Class string
	Label string
	// Open marks an interval that has not ended. It is drawn to the right edge
	// of the picture, which is not where it ends but as far as the picture
	// reaches.
	Open bool
}

// timeline is the two-lane picture of a resource: what the lifecycle says the
// resource did, above what a run metered and rated for it.
type timeline struct {
	Width int64
	Start time.Time
	End   time.Time
	Upper []lane
	Lower []lane
}

// segmentGroup is one metered interval of a resource with what its dimensions
// add up to. Rating writes one row per dimension and interval, and the picture
// draws one rectangle per interval.
type segmentGroup struct {
	State  string
	From   time.Time
	To     time.Time
	Amount decimal.Decimal
}

// scale maps instants onto the user units of one picture, from its start at
// the left edge to its end at the right one. An instant outside the span is
// drawn at the edge it lies beyond.
//
// The geometry is integer seconds and integer user units from end to end. A
// picture is not money, but a float here would make the same page render
// differently on two machines for no gain at all.
type scale struct {
	start   time.Time
	seconds int64
	width   int64
}

// newScale spans start to end over width units. A span shorter than a second,
// and one that is empty because everything happened at one instant, still has
// to divide.
func newScale(start, end time.Time, width int64) scale {
	seconds := int64(end.Sub(start) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return scale{start: start, seconds: seconds, width: width}
}

// x is where an instant falls.
func (s scale) x(t time.Time) int64 {
	seconds := int64(t.Sub(s.start) / time.Second)
	if seconds < 0 {
		return 0
	}
	if seconds > s.seconds {
		return s.width
	}
	return seconds * s.width / s.seconds
}

// buildTimeline lays both lanes out over one window: the earliest start of
// either lane to the latest end. An interval that is still open ends at now.
func buildTimeline(intervals []httpapi.LifecycleInterval, groups []segmentGroup, now time.Time) timeline {
	line := timeline{Width: chartWidth}

	start, end, ok := window(intervals, groups, now)
	if !ok {
		return line
	}
	line.Start, line.End = start, end
	sc := newScale(start, end, chartWidth)
	x := sc.x

	for _, interval := range intervals {
		drawn := lane{X: x(interval.From), Class: stateClass(interval.State), Label: interval.State}
		if interval.To == nil {
			drawn.Open = true
			drawn.Label = "still open"
			drawn.W = chartWidth - drawn.X
		} else {
			drawn.W = x(*interval.To) - drawn.X
		}
		if drawn.W < 1 {
			// A change that took less than one unit of the picture is still a
			// change the reader has to see.
			drawn.W = 1
		}
		line.Upper = append(line.Upper, drawn)
	}

	for _, group := range groups {
		drawn := lane{X: x(group.From), Class: stateClass(group.State), Label: amount(group.Amount)}
		drawn.W = x(group.To) - drawn.X
		if drawn.W < 1 {
			drawn.W = 1
		}
		line.Lower = append(line.Lower, drawn)
	}

	return line
}

// window is the span both lanes are drawn over, and reports whether there is
// anything to draw at all.
func window(intervals []httpapi.LifecycleInterval, groups []segmentGroup, now time.Time) (start, end time.Time, ok bool) {
	// add widens the window by one instant.
	add := func(at time.Time) {
		if !ok {
			start, end, ok = at, at, true
			return
		}
		if at.Before(start) {
			start = at
		}
		if at.After(end) {
			end = at
		}
	}

	for _, interval := range intervals {
		add(interval.From)
		if interval.To == nil {
			add(now)
			continue
		}
		add(*interval.To)
	}
	for _, group := range groups {
		add(group.From)
		add(group.To)
	}
	return start, end, ok
}
