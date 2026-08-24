package scheduler

import (
	"testing"
	"time"
)

// TestRetryDelay pins how long a month whose runs keep failing waits before the
// tick meters it again. The tick is hourly, and a month that will never succeed
// -- a resource whose event history carries an ordering no later event repairs
// -- would otherwise cost a full metering pass over the reporting database and
// another run row with a full stats blob every hour, forever.
func TestRetryDelay(t *testing.T) {
	for name, tc := range map[string]struct {
		failures int
		want     time.Duration
	}{
		// The month is due: nothing has failed, or the one failure that has is
		// what the next hourly tick is for.
		"a month that never failed is metered now": {failures: 0, want: 0},
		"one failure is retried at the next tick":  {failures: 1, want: 0},
		"the second failure waits two hours":       {failures: 2, want: 2 * time.Hour},
		"the third waits four":                     {failures: 3, want: 4 * time.Hour},
		"the fourth waits eight":                   {failures: 4, want: 8 * time.Hour},
		"the fifth waits sixteen":                  {failures: 5, want: 16 * time.Hour},
		// A day is the cap. Doubling on would leave a month that a fix has made
		// billable again waiting weeks for the tick to try it.
		"the sixth is capped at a day":       {failures: 6, want: maxRetryDelay},
		"a month stuck for a quarter is too": {failures: 2200, want: maxRetryDelay},
	} {
		t.Run(name, func(t *testing.T) {
			if got := retryDelay(tc.failures); got != tc.want {
				t.Errorf("retryDelay(%d) = %s, want %s", tc.failures, got, tc.want)
			}
		})
	}
}
