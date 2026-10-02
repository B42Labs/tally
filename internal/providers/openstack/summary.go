package openstack

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// Summary logs the collector's summary line once per interval.
//
// The line carries what the consumer and the sender counted since the previous
// line, whether the consumer holds a session, and what waits in the outbox. It
// is logged whether or not anything happened, so a collector at rest keeps
// reporting itself, and one cut off from the broker, one that cannot deliver
// and one that receives nothing log different lines.
type Summary struct {
	interval  time.Duration
	metrics   *Metrics
	connected func() bool
	depth     func() int64
	oldest    func() float64
	logger    *slog.Logger

	// The wait, as a field rather than a call at its site, so that the tests can
	// stand in for an elapsed interval. Nothing outside this package sets it.
	sleep func(context.Context, time.Duration) error
}

// NewSummary builds the loop that logs one summary line per interval.
//
// The counts come from m. connected, depth and oldestSeconds are read when a
// line is logged; the binary wires them to the consumer's Connected and to the
// outbox's Depth and OldestBufferedSeconds.
//
// m may be nil, which logs the counts as zeros, and logger may be nil, which
// logs through the default logger.
func NewSummary(interval time.Duration, m *Metrics, connected func() bool,
	depth func() int64, oldestSeconds func() float64, logger *slog.Logger,
) *Summary {
	if logger == nil {
		logger = slog.Default()
	}
	return &Summary{
		interval:  interval,
		metrics:   m,
		connected: connected,
		depth:     depth,
		oldest:    oldestSeconds,
		logger:    logger,
		sleep:     sleep,
	}
}

// Run logs a summary line after every interval until ctx is done, and then
// returns nil, which is the only way it returns. It logs no line when it starts
// and none when it stops: the first line follows one interval after the start
// and covers the time since then, and an interval the shutdown cut short gets
// none.
//
// The five counts are what was recorded since the previous line. The session
// and the outbox are read at the time of the line.
func (s *Summary) Run(ctx context.Context) error {
	previous := s.metrics.Totals()
	for {
		// The wait ends early only on a context that is done.
		if err := s.sleep(ctx, s.interval); err != nil {
			return nil
		}

		current := s.metrics.Totals()
		attrs := []any{
			"interval_seconds", int(s.interval / time.Second),
			"connected", s.connected(),
			"consumed", current.Consumed - previous.Consumed,
			"skipped", current.Skipped - previous.Skipped,
			"unparseable", current.Unparseable - previous.Unparseable,
			"delivered", current.Delivered - previous.Delivered,
			"delivery_errors", current.DeliveryErrors - previous.DeliveryErrors,
			"buffered", s.depth(),
		}
		// A buffer that cannot be read reports its age as NaN, which the JSON
		// handler prints as an error text in place of a number. The line then
		// carries no age.
		if oldest := s.oldest(); !math.IsNaN(oldest) {
			attrs = append(attrs, "oldest_buffered_seconds", int64(oldest))
		}
		s.logger.Info("summary", attrs...)
		previous = current
	}
}
