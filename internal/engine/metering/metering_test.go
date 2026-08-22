package metering_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/core/timeline"
	"github.com/b42labs/tally/internal/engine/invariants"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/source"
)

// The billing period every case below is metered over: March 2026, half-open,
// 44640 minutes long.
var (
	periodFrom = utc(time.March, 1)
	periodTo   = utc(time.April, 1)
)

// secondsPerMinute turns an expected minute quantity back into the seconds a
// draft carries.
var secondsPerMinute = decimal.NewFromInt(60)

// utc is a day of 2026 at midnight UTC.
func utc(month time.Month, day int) time.Time {
	return time.Date(2026, month, day, 0, 0, 0, 0, time.UTC)
}

type option func(*event.Stored)

func withState(state string) option {
	return func(s *event.Stored) { s.Payload.State = &state }
}

func withSize(size map[string]any) option {
	return func(s *event.Stored) { s.Payload.Size = size }
}

func withProject(id string) option {
	return func(s *event.Stored) { s.ProjectID = id }
}

func withResource(resourceType, resourceID string) option {
	return func(s *event.Stored) { s.ResourceType, s.ResourceID = resourceType, resourceID }
}

func withPlatform(platform string) option {
	return func(s *event.Stored) { s.Platform = platform }
}

// ev builds a stored event. Defaults keep each case to the dimension it
// exercises: one OpenStack instance of one project, received when it happened.
func ev(id, eventType string, ts time.Time, opts ...option) event.Stored {
	s := event.Stored{
		Event: event.Event{
			EventID:      id,
			Timestamp:    ts,
			EventType:    eventType,
			Platform:     "openstack",
			Cloud:        "os-prod-eu1",
			ResourceType: "instance",
			ResourceID:   "i-1",
			ProjectID:    "p-1",
			Source:       event.SourceCollector,
		},
		ReceivedAt: ts,
	}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

// size decodes a size object through the path source.History decodes the payload
// column through, so its numbers are the float64 values a draft copies verbatim.
func size(t *testing.T, object string) map[string]any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal([]byte(object), &decoded); err != nil {
		t.Fatalf("decoding the size %s: %v", object, err)
	}
	return decoded
}

// dec reads a decimal from its literal. Quantities never come from a float, and
// the forbidigo rules of .golangci.yml keep them from doing so here as well.
func dec(t *testing.T, value string) decimal.Decimal {
	t.Helper()

	d, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("reading the decimal %q: %v", value, err)
	}
	return d
}

// minutesOf is the minute quantity a draft carries.
func minutesOf(t *testing.T, draft metering.UsageDraft) decimal.Decimal {
	t.Helper()

	quantity, ok := draft.Usage["minutes"].(money.Quantity)
	if !ok {
		t.Fatalf("Usage[minutes] = %#v, want a money.Quantity", draft.Usage["minutes"])
	}
	return quantity.Decimal
}

// meterResource is MeterResource where the case under test expects it to
// succeed, which is every case but the reserved size fields below.
func meterResource(t *testing.T, history []event.Stored, from, to time.Time) []metering.UsageDraft {
	t.Helper()

	drafts, err := metering.MeterResource(history, from, to)
	if err != nil {
		t.Fatalf("MeterResource() error = %v, want nil", err)
	}
	return drafts
}

// want is one expected draft. minutes is the exact quantity as a decimal
// literal, and size the fields the interval carried, without the two the engine
// adds itself.
type want struct {
	state, project string
	from, to       time.Time
	minutes        string
	size           map[string]any
}

// wantDrafts holds a result against its expectation. Every boundary of these
// cases falls on a whole second, so a draft's seconds are exactly its minutes
// times sixty; the sub-second case asserts its own truncated seconds instead.
func wantDrafts(t *testing.T, got []metering.UsageDraft, expected []want) {
	t.Helper()

	if len(got) != len(expected) {
		t.Fatalf("got %d drafts, want %d: %+v", len(got), len(expected), got)
	}

	for i, w := range expected {
		draft := got[i]
		if draft.State != w.state {
			t.Errorf("draft %d State = %q, want %q", i, draft.State, w.state)
		}
		if draft.ProjectID != w.project {
			t.Errorf("draft %d ProjectID = %q, want %q", i, draft.ProjectID, w.project)
		}
		if !draft.FromTS.Equal(w.from) {
			t.Errorf("draft %d FromTS = %v, want %v", i, draft.FromTS, w.from)
		}
		if !draft.ToTS.Equal(w.to) {
			t.Errorf("draft %d ToTS = %v, want %v", i, draft.ToTS, w.to)
		}

		expectedMinutes := dec(t, w.minutes)
		if minutes := minutesOf(t, draft); !minutes.Equal(expectedMinutes) {
			t.Errorf("draft %d minutes = %s, want %s", i, minutes, expectedMinutes)
		}
		if seconds := expectedMinutes.Mul(secondsPerMinute); !seconds.Equal(decimal.NewFromInt(draft.Seconds)) {
			t.Errorf("draft %d Seconds = %d, want %s, the seconds its minutes are worth",
				i, draft.Seconds, seconds)
		}
		if count := draft.Usage["count"]; count != 1 {
			t.Errorf("draft %d Usage[count] = %#v, want 1", i, count)
		}

		sizeFields := maps.Clone(draft.Usage)
		delete(sizeFields, "minutes")
		delete(sizeFields, "count")
		expectedSize := w.size
		if expectedSize == nil {
			expectedSize = map[string]any{}
		}
		if !reflect.DeepEqual(sizeFields, expectedSize) {
			t.Errorf("draft %d size fields = %#v, want %#v", i, sizeFields, expectedSize)
		}
	}
}

// meterCase is one resource history and the drafts March 2026 bills it as. Each
// case runs twice: through MeterResource for its quantities, and through Meter,
// where the same drafts have to pass the invariants.
type meterCase struct {
	name    string
	history []event.Stored
	drafts  []want
}

// histories is the concept's worked examples (README.md lines 537 to 720 and
// 972) together with the lifetime, boundary, and ownership cases around them.
func histories(t *testing.T) []meterCase {
	t.Helper()

	small := `{"vcpus":2,"ram_gb":4,"disk_gb":40,"flavor":"m1.small"}`
	large := `{"vcpus":4,"ram_gb":8,"disk_gb":80,"flavor":"m1.large"}`
	cx21 := `{"vcpus":2,"ram_gb":4,"disk_gb":40,"server_type":"cx21"}`
	cx31 := `{"vcpus":4,"ram_gb":8,"disk_gb":80,"server_type":"cx31"}`
	powered := `{"vcpus":4,"ram_gb":8,"disk_gb":80}`

	return []meterCase{
		{
			name: "example 1: instance resized on 03-16",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.February, 10),
					withResource("instance", "def-456"), withProject("proj-456"),
					withState("active"), withSize(size(t, small))),
				ev("e2", "compute.instance.resize.end", utc(time.March, 16),
					withResource("instance", "def-456"), withProject("proj-456"),
					withState("active"), withSize(size(t, large))),
			},
			drafts: []want{
				{
					state: "active", project: "proj-456",
					from: periodFrom, to: utc(time.March, 16),
					minutes: "21600", size: size(t, small),
				},
				{
					state: "active", project: "proj-456",
					from: utc(time.March, 16), to: periodTo,
					minutes: "23040", size: size(t, large),
				},
			},
		},
		{
			name: "example 2: hetzner server upgraded on 03-15",
			history: []event.Stored{
				ev("e1", "server.create", utc(time.February, 20),
					withPlatform("hetzner"), withResource("server", "srv-001"),
					withState("running"), withSize(size(t, cx21))),
				ev("e2", "server.change_type", utc(time.March, 15),
					withPlatform("hetzner"), withResource("server", "srv-001"),
					withState("running"), withSize(size(t, cx31))),
			},
			drafts: []want{
				{
					state: "running", project: "p-1",
					from: periodFrom, to: utc(time.March, 15),
					minutes: "20160", size: size(t, cx21),
				},
				{
					state: "running", project: "p-1",
					from: utc(time.March, 15), to: periodTo,
					minutes: "24480", size: size(t, cx31),
				},
			},
		},
		{
			name: "example 3: volume extended on 03-10 and retyped on 03-20",
			history: []event.Stored{
				ev("e1", "volume.create.end", utc(time.February, 5),
					withResource("volume", "vol-789"),
					withState("in-use"), withSize(size(t, `{"size_gb":100,"type":"ssd"}`))),
				ev("e2", "volume.extend.end", utc(time.March, 10),
					withResource("volume", "vol-789"),
					withState("in-use"), withSize(size(t, `{"size_gb":200,"type":"ssd"}`))),
				ev("e3", "volume.retype.end", utc(time.March, 20),
					withResource("volume", "vol-789"),
					withState("in-use"), withSize(size(t, `{"size_gb":200,"type":"hdd"}`))),
			},
			drafts: []want{
				{
					state: "in-use", project: "p-1",
					from: periodFrom, to: utc(time.March, 10),
					minutes: "12960", size: size(t, `{"size_gb":100,"type":"ssd"}`),
				},
				{
					state: "in-use", project: "p-1",
					from: utc(time.March, 10), to: utc(time.March, 20),
					minutes: "14400", size: size(t, `{"size_gb":200,"type":"ssd"}`),
				},
				{
					state: "in-use", project: "p-1",
					from: utc(time.March, 20), to: periodTo,
					minutes: "17280", size: size(t, `{"size_gb":200,"type":"hdd"}`),
				},
			},
		},
		{
			// The hibernation carries no size, so the workers the shoot was
			// scaled to carry forward into the hibernated interval.
			name: "example 4: shoot scaled on 03-12, hibernated 03-25 to 03-28",
			history: []event.Stored{
				ev("e1", "shoot.create", utc(time.February, 1),
					withPlatform("gardener"), withResource("shoot", "shoot-abc"),
					withState("active"), withSize(size(t, `{"worker_count":3,"machine_type":"m1.xlarge"}`))),
				ev("e2", "shoot.worker.scale", utc(time.March, 12),
					withPlatform("gardener"), withResource("shoot", "shoot-abc"),
					withState("active"), withSize(size(t, `{"worker_count":5,"machine_type":"m1.xlarge"}`))),
				ev("e3", "shoot.hibernate", utc(time.March, 25),
					withPlatform("gardener"), withResource("shoot", "shoot-abc"),
					withState("hibernated")),
				ev("e4", "shoot.wake_up", utc(time.March, 28),
					withPlatform("gardener"), withResource("shoot", "shoot-abc"),
					withState("active")),
			},
			drafts: []want{
				{
					state: "active", project: "p-1",
					from: periodFrom, to: utc(time.March, 12),
					minutes: "15840", size: size(t, `{"worker_count":3,"machine_type":"m1.xlarge"}`),
				},
				{
					state: "active", project: "p-1",
					from: utc(time.March, 12), to: utc(time.March, 25),
					minutes: "18720", size: size(t, `{"worker_count":5,"machine_type":"m1.xlarge"}`),
				},
				{
					state: "hibernated", project: "p-1",
					from: utc(time.March, 25), to: utc(time.March, 28),
					minutes: "4320", size: size(t, `{"worker_count":5,"machine_type":"m1.xlarge"}`),
				},
				{
					state: "active", project: "p-1",
					from: utc(time.March, 28), to: periodTo,
					minutes: "5760", size: size(t, `{"worker_count":5,"machine_type":"m1.xlarge"}`),
				},
			},
		},
		{
			name: "example 5: repository storage grown on 03-18",
			history: []event.Stored{
				ev("e1", "repository.create", utc(time.February, 14),
					withPlatform("harbor"), withResource("repository", "team-alpha/app"),
					withState("active"), withSize(size(t, `{"storage_gb":10}`))),
				ev("e2", "repository.push", utc(time.March, 18),
					withPlatform("harbor"), withResource("repository", "team-alpha/app"),
					withState("active"), withSize(size(t, `{"storage_gb":15}`))),
			},
			drafts: []want{
				{
					state: "active", project: "p-1",
					from: periodFrom, to: utc(time.March, 18),
					minutes: "24480", size: size(t, `{"storage_gb":10}`),
				},
				{
					state: "active", project: "p-1",
					from: utc(time.March, 18), to: periodTo,
					minutes: "20160", size: size(t, `{"storage_gb":15}`),
				},
			},
		},
		{
			name: "power cycle: off on 03-11, on again on 03-21",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.February, 2),
					withResource("instance", "abc-123"),
					withState("active"), withSize(size(t, powered))),
				ev("e2", "compute.instance.power_off.end", utc(time.March, 11),
					withResource("instance", "abc-123"), withState("shutoff")),
				ev("e3", "compute.instance.power_on.end", utc(time.March, 21),
					withResource("instance", "abc-123"), withState("active")),
			},
			drafts: []want{
				{
					state: "active", project: "p-1",
					from: periodFrom, to: utc(time.March, 11),
					minutes: "14400", size: size(t, powered),
				},
				{
					state: "shutoff", project: "p-1",
					from: utc(time.March, 11), to: utc(time.March, 21),
					minutes: "14400", size: size(t, powered),
				},
				{
					state: "active", project: "p-1",
					from: utc(time.March, 21), to: periodTo,
					minutes: "15840", size: size(t, powered),
				},
			},
		},
		{
			name: "created inside the period",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.March, 16),
					withState("active"), withSize(size(t, small))),
			},
			drafts: []want{{
				state: "active", project: "p-1",
				from: utc(time.March, 16), to: periodTo,
				minutes: "23040", size: size(t, small),
			}},
		},
		{
			name: "deleted inside the period",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.February, 10),
					withState("active"), withSize(size(t, small))),
				ev("e2", "compute.instance.delete.end", utc(time.March, 16)),
			},
			drafts: []want{{
				state: "active", project: "p-1",
				from: periodFrom, to: utc(time.March, 16),
				minutes: "21600", size: size(t, small),
			}},
		},
		{
			name: "created and deleted inside the period",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.March, 10),
					withState("active"), withSize(size(t, small))),
				ev("e2", "compute.instance.delete.end", utc(time.March, 20)),
			},
			drafts: []want{{
				state: "active", project: "p-1",
				from: utc(time.March, 10), to: utc(time.March, 20),
				minutes: "14400", size: size(t, small),
			}},
		},
		{
			name: "alive for the whole period",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.January, 3),
					withState("active"), withSize(size(t, small))),
			},
			drafts: []want{{
				state: "active", project: "p-1",
				from: periodFrom, to: periodTo,
				minutes: "44640", size: size(t, small),
			}},
		},
		{
			// The period is half-open, so a change at its end belongs to the
			// next one and leaves this one on the configuration it started with.
			name: "a change at the period end leaves the period unsplit",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.February, 10),
					withState("active"), withSize(size(t, small))),
				ev("e2", "compute.instance.resize.end", periodTo,
					withState("active"), withSize(size(t, large))),
			},
			drafts: []want{{
				state: "active", project: "p-1",
				from: periodFrom, to: periodTo,
				minutes: "44640", size: size(t, small),
			}},
		},
		{
			name: "a change at the period start bills the whole period on the new size",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.February, 10),
					withState("active"), withSize(size(t, small))),
				ev("e2", "compute.instance.resize.end", periodFrom,
					withState("active"), withSize(size(t, large))),
			},
			drafts: []want{{
				state: "active", project: "p-1",
				from: periodFrom, to: periodTo,
				minutes: "44640", size: size(t, large),
			}},
		},
		{
			// The first event is not a create, so the resource bills from the
			// instant it is first known at and carries no size at all.
			name: "a history that starts without a create",
			history: []event.Stored{
				ev("e1", "compute.instance.power_on", utc(time.March, 5), withState("active")),
			},
			drafts: []want{{
				state: "active", project: "p-1",
				from: utc(time.March, 5), to: periodTo,
				minutes: "38880",
			}},
		},
		{
			name: "ownership transferred on 03-16",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.February, 10),
					withState("active"), withSize(size(t, small))),
				ev("e2", "compute.instance.update", utc(time.March, 16),
					withState("active"), withProject("p-2")),
			},
			drafts: []want{
				{
					state: "active", project: "p-1",
					from: periodFrom, to: utc(time.March, 16),
					minutes: "21600", size: size(t, small),
				},
				{
					state: "active", project: "p-2",
					from: utc(time.March, 16), to: periodTo,
					minutes: "23040", size: size(t, small),
				},
			},
		},
		{
			// Deleted and created again inside the period, which leaves a gap
			// the drafts are right not to close.
			name: "resurrected on 03-20",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", periodFrom,
					withState("active"), withSize(size(t, small))),
				ev("e2", "compute.instance.delete.end", utc(time.March, 10)),
				ev("e3", "compute.instance.create.end", utc(time.March, 20),
					withState("active"), withSize(size(t, small))),
			},
			drafts: []want{
				{
					state: "active", project: "p-1",
					from: periodFrom, to: utc(time.March, 10),
					minutes: "12960", size: size(t, small),
				},
				{
					state: "active", project: "p-1",
					from: utc(time.March, 20), to: periodTo,
					minutes: "17280", size: size(t, small),
				},
			},
		},
	}
}

// subSecondHistory resizes half a second into 03-16, which is the only case
// whose seconds are not its minutes times sixty.
func subSecondHistory(t *testing.T) []event.Stored {
	t.Helper()

	return []event.Stored{
		ev("e1", "compute.instance.create.end", utc(time.February, 10),
			withState("active"), withSize(size(t, `{"vcpus":2}`))),
		ev("e2", "compute.instance.resize.end",
			time.Date(2026, time.March, 16, 0, 0, 0, int(500*time.Millisecond), time.UTC),
			withState("active"), withSize(size(t, `{"vcpus":4}`))),
	}
}

func TestMeterResource(t *testing.T) {
	for _, tc := range histories(t) {
		t.Run(tc.name, func(t *testing.T) {
			wantDrafts(t, meterResource(t, tc.history, periodFrom, periodTo), tc.drafts)
		})
	}
}

// TestMeterResourceWithoutCreate pins what a resource bills whose create was
// missed: from its first event on, and with no size, because none was ever seen.
func TestMeterResourceWithoutCreate(t *testing.T) {
	drafts := meterResource(t,
		[]event.Stored{ev("e1", "compute.instance.power_on", utc(time.March, 5), withState("active"))},
		periodFrom, periodTo)

	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1: %+v", len(drafts), drafts)
	}
	if got := minutesOf(t, drafts[0]); !got.Equal(dec(t, "38880")) {
		t.Errorf("minutes = %s, want 38880", got)
	}
	if len(drafts[0].Usage) != 2 {
		t.Errorf("Usage = %#v, want only the minutes and the count", drafts[0].Usage)
	}
}

// TestMeterResourceSubSecondSplit pins decision D2 where it is visible: a
// boundary with a sub-second part truncates, so the second draft bills one
// second less than its duration and its minutes carry the four places they are
// rounded to.
func TestMeterResourceSubSecondSplit(t *testing.T) {
	drafts := meterResource(t, subSecondHistory(t), periodFrom, periodTo)

	if len(drafts) != 2 {
		t.Fatalf("got %d drafts, want 2: %+v", len(drafts), drafts)
	}
	for i, expected := range []struct {
		seconds int64
		minutes string
	}{
		{seconds: 1296000, minutes: "21600.0000"},
		{seconds: 1382399, minutes: "23039.9833"},
	} {
		if drafts[i].Seconds != expected.seconds {
			t.Errorf("draft %d Seconds = %d, want %d", i, drafts[i].Seconds, expected.seconds)
		}
		if got := minutesOf(t, drafts[i]); !got.Equal(dec(t, expected.minutes)) {
			t.Errorf("draft %d minutes = %s, want %s", i, got, expected.minutes)
		}
		encoded, err := json.Marshal(drafts[i].Usage["minutes"])
		if err != nil {
			t.Fatalf("encoding the minutes of draft %d: %v", i, err)
		}
		if string(encoded) != expected.minutes {
			t.Errorf("draft %d minutes encoded as %s, want %s", i, encoded, expected.minutes)
		}
	}
}

// TestMeterResourceBillsNothing collects the histories and the periods that bill
// nothing at all. Each has to yield an empty slice rather than a nil one, so a
// caller ranging over the result meets the same value either way.
func TestMeterResourceBillsNothing(t *testing.T) {
	small := size(t, `{"vcpus":2}`)
	alive := []event.Stored{
		ev("e1", "compute.instance.create.end", utc(time.February, 10),
			withState("active"), withSize(small)),
	}

	tests := []struct {
		name     string
		history  []event.Stored
		from, to time.Time
	}{
		{name: "a nil history", history: nil, from: periodFrom, to: periodTo},
		{name: "an empty history", history: []event.Stored{}, from: periodFrom, to: periodTo},
		{
			name: "deleted before the period",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.January, 5),
					withState("active"), withSize(small)),
				ev("e2", "compute.instance.delete.end", utc(time.February, 10)),
			},
			from: periodFrom, to: periodTo,
		},
		{
			name: "created at the period end",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", periodTo,
					withState("active"), withSize(small)),
			},
			from: periodFrom, to: periodTo,
		},
		{
			name: "created after the period end",
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", utc(time.April, 15),
					withState("active"), withSize(small)),
			},
			from: periodFrom, to: periodTo,
		},
		{
			// The create predates what the events table still holds, so the
			// delete is the only event there is.
			name: "a history that is nothing but a delete",
			history: []event.Stored{
				ev("e1", "compute.instance.delete.end", utc(time.March, 10)),
			},
			from: periodFrom, to: periodTo,
		},
		{name: "a period of no length", history: alive, from: periodFrom, to: periodFrom},
		{name: "a period that ends before it starts", history: alive, from: periodTo, to: periodFrom},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := meterResource(t, tc.history, tc.from, tc.to)

			if got == nil {
				t.Fatal("MeterResource() = nil, want an empty slice")
			}
			if len(got) != 0 {
				t.Errorf("MeterResource() = %+v, want no drafts", got)
			}
		})
	}
}

// TestUsageJSON pins the object a usage row is written as: the size fields as
// the provider sent them, and the two keys the engine adds, with the minutes at
// the four places they are rounded to.
func TestUsageJSON(t *testing.T) {
	drafts := meterResource(t, []event.Stored{
		ev("e1", "compute.instance.create.end", utc(time.February, 10), withState("active"),
			withSize(size(t, `{"vcpus":2,"ram_gb":4,"disk_gb":40,"flavor":"m1.small"}`))),
		ev("e2", "compute.instance.resize.end", utc(time.March, 16), withState("active"),
			withSize(size(t, `{"vcpus":4,"ram_gb":8,"disk_gb":80,"flavor":"m1.large"}`))),
	}, periodFrom, periodTo)
	if len(drafts) == 0 {
		t.Fatal("MeterResource() returned no drafts")
	}

	encoded, err := json.Marshal(drafts[0].Usage)
	if err != nil {
		t.Fatalf("encoding the usage: %v", err)
	}
	expected := `{"count":1,"disk_gb":40,"flavor":"m1.small","minutes":21600.0000,"ram_gb":4,"vcpus":2}`
	if string(encoded) != expected {
		t.Errorf("usage = %s, want %s", encoded, expected)
	}
}

// TestUsageRefusesAReservedSizeField pins what a size field the engine names
// itself does: it fails the resource rather than being merged. Neither order
// bills the right number. Overwriting it drops a count the customer pays for —
// a bucket reporting 47 objects would be billed as one, and the implicit count
// invariant would confirm the one, because it reads the field the fold
// overwrote. Keeping it bills a quantity nothing here derived.
func TestUsageRefusesAReservedSizeField(t *testing.T) {
	for _, reserved := range []string{"minutes", "count"} {
		t.Run("a size carrying "+reserved, func(t *testing.T) {
			history := []event.Stored{
				ev("e1", "compute.instance.create.end", periodFrom, withState("active"),
					withSize(size(t, `{"`+reserved+`":47,"vcpus":2}`))),
			}

			drafts, err := metering.MeterResource(history, periodFrom, periodTo)
			if err == nil {
				t.Fatalf("MeterResource() = %+v, want the reserved field refused", drafts)
			}
			if quoted := `"` + reserved + `"`; !strings.Contains(err.Error(), quoted) {
				t.Errorf("MeterResource() error = %q, want it to name %s", err, quoted)
			}

			// The same size read through the loop fails the pass too, as a
			// violation of the resource it was read from rather than as a bare
			// error: it reaches an operator through the run's stats, beside
			// every other resource of the period that cannot be billed.
			result, err := metering.Meter(t.Context(), fakeOf(history), periodFrom, periodTo, nil)
			if result != nil {
				t.Errorf("Meter() = %+v, want no output", result)
			}
			var reported *metering.ViolationError
			if !errors.As(err, &reported) {
				t.Fatalf("Meter() error = %v, want a *metering.ViolationError", err)
			}
			if len(reported.Resources) != 1 || reported.Resources[0].ResourceID != "i-1" {
				t.Fatalf("violating resources = %+v, want the one the size was read from", reported.Resources)
			}
			violations := reported.Resources[0].Violations
			if len(violations) != 1 || violations[0].Invariant != metering.InvariantReservedUsageField {
				t.Fatalf("violations = %+v, want one %s", violations, metering.InvariantReservedUsageField)
			}
			if quoted := `"` + reserved + `"`; !strings.Contains(violations[0].Detail, quoted) {
				t.Errorf("violation detail = %q, want it to name %s", violations[0].Detail, quoted)
			}
		})
	}
}

// TestMeterReportsEveryResourceWithAReservedSizeField pins that one such size
// does not stop the period at the resource carrying it. The size sits in an
// append-only events table and the fold carries it forward, so every later
// period meets it again: a run that returned on the first one would name one
// resource per run and have an operator work through a cloud's resources one
// run at a time, while the clouds behind it went unbilled for just as long.
func TestMeterReportsEveryResourceWithAReservedSizeField(t *testing.T) {
	poisoned := func(id string) []event.Stored {
		return []event.Stored{
			ev("e1-"+id, "objectstore.bucket.update", periodFrom,
				withResource("bucket", id), withState("active"),
				withSize(size(t, `{"count":47}`))),
		}
	}
	sound := []event.Stored{
		ev("e1-sound", "compute.instance.create.end", utc(time.February, 10),
			withResource("instance", "sound"), withState("active"), withSize(size(t, `{"vcpus":2}`))),
	}

	src := fakeOf(poisoned("bucket-a"), sound, poisoned("bucket-b"))

	result, err := metering.Meter(t.Context(), src, periodFrom, periodTo, nil)
	if result != nil {
		t.Errorf("Meter() = %+v, want no partial output", result)
	}

	var reported *metering.ViolationError
	if !errors.As(err, &reported) {
		t.Fatalf("Meter() error = %v, want a *metering.ViolationError", err)
	}
	got := make([]string, 0, len(reported.Resources))
	for _, resource := range reported.Resources {
		got = append(got, resource.ResourceID)
		if !slices.ContainsFunc(resource.Violations, func(v invariants.Violation) bool {
			return v.Invariant == metering.InvariantReservedUsageField
		}) {
			t.Errorf("violations of %s = %+v, want a %s among them",
				resource.ResourceID, resource.Violations, metering.InvariantReservedUsageField)
		}
	}
	if expected := []string{"bucket-a", "bucket-b"}; !slices.Equal(got, expected) {
		t.Errorf("violating resources = %v, want %v, in candidate order", got, expected)
	}
}

// The errors the fake source fails a read with, which Meter has to return as it
// received them.
var (
	errCandidates = errors.New("listing the candidate resources failed")
	errHistory    = errors.New("loading the history failed")
)

// fakeSource is the metering loop's source in memory: a candidate list, a
// history per candidate, and the errors either read can fail with. A candidate
// without an entry in histories has no events, which is what the projection
// listing a resource the events table never saw looks like.
type fakeSource struct {
	candidates    []source.Resource
	histories     map[source.Resource][]event.Stored
	candidatesErr error
	historyErr    error
}

func (f *fakeSource) Candidates(ctx context.Context, _ []string, _, _ time.Time) ([]source.Resource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.candidatesErr != nil {
		return nil, f.candidatesErr
	}
	return f.candidates, nil
}

func (f *fakeSource) History(ctx context.Context, r source.Resource, _ time.Time) ([]event.Stored, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return f.histories[r], nil
}

// resourceOf is the candidate a history belongs to.
func resourceOf(e event.Stored) source.Resource {
	return source.Resource{
		Cloud: e.Cloud, Platform: e.Platform, ResourceType: e.ResourceType, ResourceID: e.ResourceID,
	}
}

// fakeOf builds a source listing every history's resource, in the order they
// were given.
func fakeOf(hs ...[]event.Stored) *fakeSource {
	f := &fakeSource{histories: make(map[source.Resource][]event.Stored, len(hs))}
	for _, history := range hs {
		r := resourceOf(history[0])
		f.candidates = append(f.candidates, r)
		f.histories[r] = history
	}
	return f
}

// TestMeterAcceptsEveryWorkedExample runs every history through the whole loop.
// None may breach an invariant: the spans metering derives have to hold against
// the lives the same events imply, which is checked independently of the fold.
func TestMeterAcceptsEveryWorkedExample(t *testing.T) {
	cases := histories(t)
	cases = append(cases, meterCase{name: "a sub-second split", history: subSecondHistory(t)})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fakeOf(tc.history)

			result, err := metering.Meter(t.Context(), src, periodFrom, periodTo, []string{"os-prod-eu1"})
			if err != nil {
				t.Fatalf("Meter() error = %v, want nil", err)
			}
			if result.Candidates != 1 {
				t.Errorf("Candidates = %d, want 1", result.Candidates)
			}
			if len(result.Resources) != 1 {
				t.Fatalf("got %d resources, want 1: %+v", len(result.Resources), result.Resources)
			}
			if tc.drafts != nil {
				wantDrafts(t, result.Resources[0].Drafts, tc.drafts)
			}
		})
	}
}

// TestMeterWalksTheCandidates pins what the loop does with candidates that bill
// nothing: it counts them, warns about the one without a history, and leaves
// both out of the usage it hands on.
func TestMeterWalksTheCandidates(t *testing.T) {
	small := size(t, `{"vcpus":2}`)
	billing := func(id string) []event.Stored {
		return []event.Stored{
			ev("e1-"+id, "compute.instance.create.end", utc(time.February, 10),
				withResource("instance", id), withState("active"), withSize(small)),
		}
	}
	gone := []event.Stored{
		ev("e1-gone", "compute.instance.create.end", utc(time.January, 5),
			withResource("instance", "gone"), withState("active"), withSize(small)),
		ev("e2-gone", "compute.instance.delete.end", utc(time.February, 10),
			withResource("instance", "gone")),
	}
	empty := source.Resource{
		Cloud: "os-prod-eu1", Platform: "openstack", ResourceType: "instance", ResourceID: "no-events",
	}

	src := fakeOf(gone, billing("second"), billing("first"))
	src.candidates = slices.Insert(src.candidates, 2, empty)

	result, err := metering.Meter(t.Context(), src, periodFrom, periodTo, nil)
	if err != nil {
		t.Fatalf("Meter() error = %v, want nil", err)
	}

	if result.Candidates != 4 {
		t.Errorf("Candidates = %d, want 4, every candidate examined", result.Candidates)
	}
	gotIDs := make([]string, 0, len(result.Resources))
	for _, usage := range result.Resources {
		gotIDs = append(gotIDs, usage.Resource.ResourceID)
	}
	if expected := []string{"second", "first"}; !slices.Equal(gotIDs, expected) {
		t.Errorf("resources = %v, want %v, in candidate order", gotIDs, expected)
	}

	expectedWarnings := []metering.Warning{{
		Cloud: "os-prod-eu1", ResourceType: "instance", ResourceID: "no-events",
		Code: metering.WarningCandidateWithoutHistory,
	}}
	if !reflect.DeepEqual(result.Warnings, expectedWarnings) {
		t.Errorf("Warnings = %+v, want %+v", result.Warnings, expectedWarnings)
	}
}

// TestMeterWarnsAboutAMissingCreate pins that the fold's warning reaches the
// result named after the resource it belongs to, and that the resource is still
// billed from the first event there is.
func TestMeterWarnsAboutAMissingCreate(t *testing.T) {
	src := fakeOf([]event.Stored{
		ev("e1", "compute.instance.power_on", utc(time.March, 5), withState("active")),
	})

	result, err := metering.Meter(t.Context(), src, periodFrom, periodTo, nil)
	if err != nil {
		t.Fatalf("Meter() error = %v, want nil", err)
	}

	expected := []metering.Warning{{
		Cloud: "os-prod-eu1", ResourceType: "instance", ResourceID: "i-1",
		Code: timeline.WarningHistoryStartsWithoutCreate,
	}}
	if !reflect.DeepEqual(result.Warnings, expected) {
		t.Errorf("Warnings = %+v, want %+v", result.Warnings, expected)
	}
	if len(result.Resources) != 1 || len(result.Resources[0].Drafts) != 1 {
		t.Fatalf("Resources = %+v, want the resource billed from its first event", result.Resources)
	}
}

// TestWarningJSONShape pins the object a warning is written as. The warnings
// are serialized into runs.stats, so the field names are what an operator reads
// and greps for, the same as the violation report's.
func TestWarningJSONShape(t *testing.T) {
	got, err := json.Marshal(metering.Warning{
		Cloud:        "os-prod-eu1",
		ResourceType: "instance",
		ResourceID:   "i-1",
		Code:         metering.WarningCandidateWithoutHistory,
	})
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}
	want := `{"cloud":"os-prod-eu1","resource_type":"instance","resource_id":"i-1","code":"candidate_without_history"}`
	if string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
}

// TestMeterMetersNothing pins the empty period: a projection with no candidate
// in it is a run that meters nothing rather than a run that failed.
func TestMeterMetersNothing(t *testing.T) {
	result, err := metering.Meter(t.Context(), &fakeSource{}, periodFrom, periodTo, nil)
	if err != nil {
		t.Fatalf("Meter() error = %v, want nil", err)
	}
	if result.Candidates != 0 {
		t.Errorf("Candidates = %d, want 0", result.Candidates)
	}
	if len(result.Resources) != 0 {
		t.Errorf("Resources = %+v, want none", result.Resources)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("Warnings = %+v, want none", result.Warnings)
	}
}

// TestMeterReportsEveryViolatingResource pins the failure contract: the loop
// keeps walking so the report is complete, and nothing at all comes back with
// it, because a period is either wholly metered or not metered.
func TestMeterReportsEveryViolatingResource(t *testing.T) {
	small := size(t, `{"vcpus":2}`)
	// An update after the delete reopens the timeline, which bills time past
	// the only life the events describe.
	broken := func(id string) []event.Stored {
		return []event.Stored{
			ev("e1-"+id, "compute.instance.create.end", periodFrom,
				withResource("instance", id), withState("active"), withSize(small)),
			ev("e2-"+id, "compute.instance.delete.end", utc(time.March, 10),
				withResource("instance", id)),
			ev("e3-"+id, "compute.instance.update", utc(time.March, 20),
				withResource("instance", id), withState("active")),
		}
	}
	sound := []event.Stored{
		ev("e1-sound", "compute.instance.create.end", utc(time.February, 10),
			withResource("instance", "sound"), withState("active"), withSize(small)),
	}

	src := fakeOf(broken("bad-first"), sound, broken("bad-second"))

	result, err := metering.Meter(t.Context(), src, periodFrom, periodTo, nil)
	if result != nil {
		t.Errorf("Meter() = %+v, want no partial output", result)
	}

	var reported *metering.ViolationError
	if !errors.As(err, &reported) {
		t.Fatalf("Meter() error = %v, want a *metering.ViolationError", err)
	}
	if expected := "2 resources violate the metering invariants"; err.Error() != expected {
		t.Errorf("Meter() error = %q, want %q", err, expected)
	}

	gotIDs := make([]string, 0, len(reported.Resources))
	for _, resource := range reported.Resources {
		gotIDs = append(gotIDs, resource.ResourceID)
		if resource.Cloud != "os-prod-eu1" || resource.ResourceType != "instance" {
			t.Errorf("violating resource = %+v, want it named by cloud and type", resource)
		}
		if len(resource.Violations) == 0 {
			t.Errorf("violations of %s = none, want the breach reported", resource.ResourceID)
		}
		if !slices.ContainsFunc(resource.Violations, func(v invariants.Violation) bool {
			return v.Invariant == invariants.InvariantCoverage
		}) {
			t.Errorf("violations of %s = %+v, want a %s breach among them",
				resource.ResourceID, resource.Violations, invariants.InvariantCoverage)
		}
	}
	if expected := []string{"bad-first", "bad-second"}; !slices.Equal(gotIDs, expected) {
		t.Errorf("violating resources = %v, want %v, in candidate order", gotIDs, expected)
	}

	encoded, err := json.Marshal(reported.Resources[0])
	if err != nil {
		t.Fatalf("encoding the violation report: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decoding the violation report: %v", err)
	}
	keys := slices.Sorted(maps.Keys(fields))
	if expected := []string{"cloud", "resource_id", "resource_type", "violations"}; !slices.Equal(keys, expected) {
		t.Errorf("violation report keys = %v, want %v", keys, expected)
	}
}

// TestMeterReturnsSourceErrorsUnchanged pins that a failed read comes back as
// the source reported it: the run classifies a canceled context apart from a
// query that failed, and it can only do so while the error is unwrapped.
func TestMeterReturnsSourceErrorsUnchanged(t *testing.T) {
	history := []event.Stored{
		ev("e1", "compute.instance.create.end", utc(time.February, 10),
			withState("active"), withSize(size(t, `{"vcpus":2}`))),
	}

	t.Run("from the candidate list", func(t *testing.T) {
		src := fakeOf(history)
		src.candidatesErr = errCandidates

		_, err := metering.Meter(t.Context(), src, periodFrom, periodTo, nil)
		if !errors.Is(err, errCandidates) {
			t.Fatalf("Meter() error = %v, want %v", err, errCandidates)
		}
		if err != errCandidates {
			t.Errorf("Meter() error = %v, want it returned unwrapped", err)
		}
	})

	t.Run("from a history", func(t *testing.T) {
		src := fakeOf(history)
		src.historyErr = errHistory

		_, err := metering.Meter(t.Context(), src, periodFrom, periodTo, nil)
		if !errors.Is(err, errHistory) {
			t.Fatalf("Meter() error = %v, want %v", err, errHistory)
		}
		if err != errHistory {
			t.Errorf("Meter() error = %v, want it returned unwrapped", err)
		}
	})

	t.Run("from a canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := metering.Meter(ctx, fakeOf(history), periodFrom, periodTo, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Meter() error = %v, want it to be context.Canceled", err)
		}
	})
}
