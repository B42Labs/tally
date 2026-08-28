package simulator

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"
)

// The months the workload tests generate. July and May are both 31 days long,
// so a comparison between them holds every instant at the same offset, while
// June is a day shorter and moves the ones anchored on the month's end.
var (
	july2026 = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	june2026 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	may2026  = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
)

// testCloud is the cloud the generated months are salted with.
const testCloud = "os-test"

// collectorTopic and collectorExchanges are what the collector binds its queue
// to out of the box: the defaults of TALLY_OSC_TOPICS and TALLY_OSC_EXCHANGES
// in internal/providers/openstack/config.go. A simulator that addressed
// anything else would publish a month no collector receives.
const collectorTopic = "notifications.info"

var collectorExchanges = []string{"nova", "cinder", "neutron", "glance"}

// generateMonth generates one month or fails the test. Every test takes its
// schedule through it, so a generation error is reported once and in one form.
func generateMonth(t *testing.T, seed uint64, from time.Time, cloud string) Schedule {
	t.Helper()

	schedule, err := Generate(seed, from, from.AddDate(0, 1, 0), cloud)
	if err != nil {
		t.Fatalf("Generate(%d, %s, %q) error = %v, want nil", seed, from.Format(time.RFC3339), cloud, err)
	}
	return schedule
}

// requireDisjoint fails the test when the two schedules share a value of the
// field, which is what salting the identifiers is supposed to rule out.
func requireDisjoint(t *testing.T, field string, first, second Schedule, of func(Transition) string) {
	t.Helper()

	seen := make(map[string]struct{}, len(first))
	for _, transition := range first {
		seen[of(transition)] = struct{}{}
	}
	for _, transition := range second {
		if _, ok := seen[of(transition)]; ok {
			t.Errorf("both schedules carry the %s %q, want them disjoint", field, of(transition))
			return
		}
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	first := generateMonth(t, 7, july2026, testCloud)
	second := generateMonth(t, 7, july2026, testCloud)

	if len(first) != len(second) {
		t.Fatalf("the same seed produced %d and %d transitions, want the same month twice",
			len(first), len(second))
	}
	for i := range first {
		before, after := render(t, first[i]), render(t, second[i])
		if !bytes.Equal(before, after) {
			t.Fatalf("transition %d differs between two runs of the same seed:\n%s\n%s", i, before, after)
		}
	}
}

func TestGenerateSaltsIdentifiersWithPeriodAndCloud(t *testing.T) {
	t.Run("another cloud keeps the shape and renames everything", func(t *testing.T) {
		first := generateMonth(t, 7, july2026, "os-a")
		second := generateMonth(t, 7, july2026, "os-b")

		if len(first) != len(second) {
			t.Fatalf("two clouds produced %d and %d transitions, want the shape to be the seed's alone",
				len(first), len(second))
		}
		for i := range first {
			if first[i].EventType != second[i].EventType {
				t.Errorf("transition %d is %s on one cloud and %s on the other, want the same shape",
					i, first[i].EventType, second[i].EventType)
			}
			if !first[i].At.Equal(second[i].At) {
				t.Errorf("transition %d is at %s on one cloud and %s on the other, want the same shape",
					i, first[i].At, second[i].At)
			}
			if len(first[i].Payload) != len(second[i].Payload) {
				t.Errorf("transition %d carries %d payload members on one cloud and %d on the other",
					i, len(first[i].Payload), len(second[i].Payload))
			}
		}
		requireDisjoint(t, "message id", first, second, func(tr Transition) string { return tr.MessageID })
		requireDisjoint(t, "resource id", first, second, func(tr Transition) string { return tr.ResourceID })
		requireDisjoint(t, "project id", first, second, func(tr Transition) string { return tr.ProjectID })
	})

	t.Run("another month of the same length keeps every offset", func(t *testing.T) {
		july := generateMonth(t, 7, july2026, testCloud)
		may := generateMonth(t, 7, may2026, testCloud)

		if len(july) != len(may) {
			t.Fatalf("two 31 day months produced %d and %d transitions, want the same shape",
				len(july), len(may))
		}
		for i := range july {
			if july[i].EventType != may[i].EventType {
				t.Errorf("transition %d is %s in July and %s in May, want the same shape",
					i, july[i].EventType, may[i].EventType)
			}
			inJuly, inMay := july[i].At.Sub(july2026), may[i].At.Sub(may2026)
			if inJuly != inMay {
				t.Errorf("transition %d sits %s into July and %s into May, want the same offset",
					i, inJuly, inMay)
			}
		}
		requireDisjoint(t, "message id", july, may, func(tr Transition) string { return tr.MessageID })
	})

	t.Run("a shorter month renames everything as well", func(t *testing.T) {
		// June is a day shorter than July, so the transitions anchored on the end
		// of the month sit at other offsets and the offsets are not compared.
		july := generateMonth(t, 7, july2026, testCloud)
		june := generateMonth(t, 7, june2026, testCloud)

		requireDisjoint(t, "message id", july, june, func(tr Transition) string { return tr.MessageID })
		requireDisjoint(t, "resource id", july, june, func(tr Transition) string { return tr.ResourceID })
	})
}

func TestGenerateStaysInsideThePeriod(t *testing.T) {
	from := july2026
	to := from.AddDate(0, 1, 0)
	schedule := generateMonth(t, 1, from, testCloud)

	if routingKey != collectorTopic {
		t.Errorf("routingKey = %q, want %q, the topic the collector binds by default",
			routingKey, collectorTopic)
	}

	last := make(map[string]time.Time, len(schedule))
	for i, transition := range schedule {
		if transition.At.Before(from) || !transition.At.Before(to) {
			t.Errorf("transition %d (%s) is at %s, want it inside [%s, %s)",
				i, transition.EventType, transition.At, from, to)
		}
		if i > 0 && transition.At.Before(schedule[i-1].At) {
			t.Errorf("transition %d is at %s after %s, want the schedule sorted",
				i, transition.At, schedule[i-1].At)
		}
		if !slices.Contains(collectorExchanges, transition.Exchange) {
			t.Errorf("%s is published on %q, want one of the exchanges the collector binds: %v",
				transition.EventType, transition.Exchange, collectorExchanges)
		}
		if previous, ok := last[transition.ResourceID]; ok && transition.At.Sub(previous) < time.Second {
			t.Errorf("resource %s reports %s at %s, %s after its previous transition, want at least a second",
				transition.ResourceID, transition.EventType, transition.At, transition.At.Sub(previous))
		}
		last[transition.ResourceID] = transition.At
	}

	var want []Transition
	for _, transition := range schedule {
		if transition.EventType != "image.create" {
			want = append(want, transition)
		}
	}
	got := schedule.Billable()
	if len(got) != len(want) {
		t.Fatalf("Billable() returned %d of %d transitions, want the %d that are not an image.create",
			len(got), len(schedule), len(want))
	}
	for i := range got {
		if got[i].MessageID != want[i].MessageID {
			t.Errorf("Billable()[%d] is %s, want %s", i, got[i].MessageID, want[i].MessageID)
		}
	}
}

func TestGenerateRefusesANonMonth(t *testing.T) {
	cases := []struct {
		name string
		from time.Time
		to   time.Time
	}{
		{
			name: "a period that starts mid-month",
			from: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "a period that is not the whole month",
			from: july2026,
			to:   july2026.AddDate(0, 0, 20),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Generate(1, c.from, c.to, testCloud)
			if err == nil {
				t.Fatalf("Generate(1, %s, %s, %q) error = nil, want a refusal",
					c.from.Format(time.RFC3339), c.to.Format(time.RFC3339), testCloud)
			}
			if !strings.HasSuffix(err.Error(), " is not a UTC month") {
				t.Errorf("Generate() error = %q, want it to end with %q", err, " is not a UTC month")
			}
		})
	}
}
