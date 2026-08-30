package simulator

import (
	"fmt"
	"testing"
	"time"
)

// TestEveryBillableTransitionIsBookedOnce holds the fact ledger against the
// schedule it is built beside. The effect of a transition is stated at the emit
// that produces it, so an emit handed the unbooked effect by mistake leaves the
// month with a billable transition the oracle says nothing about. The
// comparison would then report the engine at fault for a resource nobody stated
// a size for, which is the one failure a comparison must never invent.
//
// The oslo type of every fact is held against billableTypes as well, so an emit
// that booked a type the collector records nothing for is caught here too.
func TestEveryBillableTransitionIsBookedOnce(t *testing.T) {
	// booking is one transition as the ledger and the schedule both name it: the
	// instant it happened at and the resource it is about. The instant alone
	// would not do, since two resources of a month change in the same second.
	type booking struct {
		at time.Time
		id string
	}

	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			g := buildMonth(t, seed)
			billable := g.schedule.Billable()

			if len(g.facts) != len(billable) {
				t.Errorf("the month books %d facts over %d billable transitions, want one fact per "+
					"transition", len(g.facts), len(billable))
			}

			booked := make(map[booking]int, len(g.facts))
			for _, f := range g.facts {
				booked[booking{at: f.at, id: f.resourceID}]++
				if f.eventType == imageCreateType {
					t.Errorf("%s at %s is booked, want the one type the mapping skips to stay out of "+
						"the ledger: the image has no size yet", f.eventType, f.at.Format(time.RFC3339))
					continue
				}
				if _, ok := billableTypes[f.eventType]; !ok {
					t.Errorf("%s at %s is booked under a type the ledger does not name, want every "+
						"fact to carry one of the %d types billableTypes holds", f.eventType,
						f.at.Format(time.RFC3339), len(billableTypes))
				}
			}
			emitted := make(map[booking]int, len(billable))
			for _, transition := range billable {
				emitted[booking{at: transition.At, id: transition.ResourceID}]++
			}

			for b, count := range emitted {
				if booked[b] != count {
					t.Errorf("resource %s at %s is emitted %d times and booked %d times, want the "+
						"ledger to state the effect of every billable transition", b.id,
						b.at.Format(time.RFC3339), count, booked[b])
				}
			}
			for b, count := range booked {
				if _, ok := emitted[b]; !ok {
					t.Errorf("resource %s at %s is booked %d times and emitted never, want the ledger "+
						"to state nothing the month did not do", b.id, b.at.Format(time.RFC3339), count)
				}
			}
		})
	}
}
