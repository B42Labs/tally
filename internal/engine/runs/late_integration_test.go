package runs_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/source"
)

// detectLate reports the late events of one period against the fixture, on the
// test's own context.
func (f fixture) detectLate(t *testing.T, from, to time.Time) (runs.LateReport, error) {
	t.Helper()

	return runs.DetectLate(t.Context(), f.engine.Store.Pool(), f.source, from, to)
}

// storedSnapshotAt is the instant a run recorded as its snapshot_at, read back
// out of its stats. It is parsed at the precision the stats hold it in, because
// the cases below seed events around it and a dropped digit would move the
// bound they are held against.
func (f fixture) storedSnapshotAt(t *testing.T, runID uuid.UUID) time.Time {
	t.Helper()

	stored := text(t, f.readStats(t, runID), "snapshot_at")
	at, err := time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		t.Fatalf("parsing the snapshot_at %q of run %s: %v", stored, runID, err)
	}
	return at
}

// TestDetectLate reports the late events of finalized periods. Every case keeps
// to its own month and its own cloud: the late events of a period are counted
// over every cloud, so two cases sharing a month would read each other's
// events. The case that dates events outside its period takes a month whose
// neighbours no other case bills, for the same reason.
func TestDetectLate(t *testing.T) {
	f := newFixture(t)

	t.Run("lists the resources whose events arrived after the run read them", func(t *testing.T) {
		from, to := month(-8)
		const cloud = "os-late-listed"
		f.seedProject(t, cloud, "proj-listed")
		vm := instance(cloud, "i-late", "proj-listed", from, standardSize)
		f.seedResource(t, vm)

		finalized := f.closedMonth(t, from, to, cloud)
		snapshotAt := f.storedSnapshotAt(t, finalized)

		// A power cycle dated inside the resource's life and received now,
		// which is past the instant the finalized run read the events at.
		f.seedEvent(t, vm, "ev-off-i-late", "compute.instance.power_off.end",
			vm.created.Add(12*time.Hour), `{"state":"shutoff"}`)
		f.seedEvent(t, vm, "ev-on-i-late", "compute.instance.power_on.end",
			vm.created.Add(24*time.Hour), `{"state":"active"}`)

		got, err := f.detectLate(t, from, to)
		if err != nil {
			t.Fatalf("DetectLate() error = %v, want nil", err)
		}
		if got.RunID != finalized {
			t.Errorf("RunID = %s, want the finalized run %s", got.RunID, finalized)
		}
		if got.Kind != runs.KindRegular {
			t.Errorf("Kind = %q, want %q", got.Kind, runs.KindRegular)
		}
		if !got.SnapshotAt.Equal(snapshotAt) {
			t.Errorf("SnapshotAt = %s, want the %s the run stored", got.SnapshotAt, snapshotAt)
		}
		if got.SnapshotAt.Location() != time.UTC {
			t.Errorf("SnapshotAt zone = %v, want UTC", got.SnapshotAt.Location())
		}

		if len(got.Resources) != 1 {
			t.Fatalf("Resources = %+v, want the one resource the power cycle belongs to", got.Resources)
		}
		late := got.Resources[0]
		want := source.Resource{
			Cloud: cloud, Platform: platform, ResourceType: resourceType, ResourceID: vm.id,
		}
		if late.Resource != want {
			t.Errorf("Resource = %+v, want %+v", late.Resource, want)
		}
		if late.Events != 2 {
			t.Errorf("Events = %d, want the two events of the power cycle", late.Events)
		}
		if !late.LastReceivedAt.After(got.SnapshotAt) {
			t.Errorf("LastReceivedAt = %s, want an instant past the snapshot %s",
				late.LastReceivedAt, got.SnapshotAt)
		}
	})

	t.Run("reports nothing late for a month the run saw whole", func(t *testing.T) {
		from, to := month(-9)
		const cloud = "os-late-clean"
		f.seedProject(t, cloud, "proj-clean")
		f.seedResource(t, instance(cloud, "i-clean", "proj-clean", from, standardSize))

		finalized := f.closedMonth(t, from, to, cloud)

		got, err := f.detectLate(t, from, to)
		if err != nil {
			t.Fatalf("DetectLate() error = %v, want nil", err)
		}
		if got.RunID != finalized {
			t.Errorf("RunID = %s, want the finalized run %s", got.RunID, finalized)
		}
		if got.Resources == nil {
			t.Fatal("Resources = nil, want the empty list a period nothing reached late reports")
		}
		if len(got.Resources) != 0 {
			t.Errorf("Resources = %+v, want none: the run read every event of the month", got.Resources)
		}
	})

	t.Run("ignores events the run had already read", func(t *testing.T) {
		from, to := month(-10)
		const cloud = "os-late-bound"
		f.seedProject(t, cloud, "proj-bound")
		vm := instance(cloud, "i-bound", "proj-bound", from, standardSize)
		f.seedResource(t, vm)

		finalized := f.closedMonth(t, from, to, cloud)
		snapshotAt := f.storedSnapshotAt(t, finalized)

		// The bound is strict, so an event received at the snapshot instant is
		// one the run read, and so is anything older.
		f.seedEventReceived(t, vm, "ev-at-i-bound", "compute.instance.update",
			vm.created.Add(time.Hour), snapshotAt, `{"state":"active"}`)
		f.seedEventReceived(t, vm, "ev-before-i-bound", "compute.instance.update",
			vm.created.Add(2*time.Hour), snapshotAt.Add(-time.Hour), `{"state":"active"}`)

		got, err := f.detectLate(t, from, to)
		if err != nil {
			t.Fatalf("DetectLate() error = %v, want nil", err)
		}
		if len(got.Resources) != 0 {
			t.Errorf("Resources = %+v, want none: the run read both events", got.Resources)
		}

		f.seedEventReceived(t, vm, "ev-after-i-bound", "compute.instance.update",
			vm.created.Add(3*time.Hour), snapshotAt.Add(time.Second), `{"state":"active"}`)

		got, err = f.detectLate(t, from, to)
		if err != nil {
			t.Fatalf("DetectLate() error = %v, want nil", err)
		}
		if len(got.Resources) != 1 {
			t.Fatalf("Resources = %+v, want the resource the event a second past the snapshot belongs to",
				got.Resources)
		}
		if events := got.Resources[0].Events; events != 1 {
			t.Errorf("Events = %d, want the one event received past the snapshot", events)
		}
	})

	t.Run("ignores events dated outside the period", func(t *testing.T) {
		from, to := month(-20)
		const cloud = "os-late-outside"
		f.seedProject(t, cloud, "proj-outside")
		vm := instance(cloud, "i-outside", "proj-outside", from, standardSize)
		f.seedResource(t, vm)
		f.closedMonth(t, from, to, cloud)

		// Both received now, so both past the snapshot, and both dated outside
		// the half-open period: one an hour before it begins, one at the
		// instant it ends.
		f.seedEvent(t, vm, "ev-before-i-outside", "compute.instance.update",
			from.Add(-time.Hour), `{"state":"active"}`)
		f.seedEvent(t, vm, "ev-at-end-i-outside", "compute.instance.update",
			to, `{"state":"active"}`)

		got, err := f.detectLate(t, from, to)
		if err != nil {
			t.Fatalf("DetectLate() error = %v, want nil", err)
		}
		if len(got.Resources) != 0 {
			t.Errorf("Resources = %+v, want none: neither event is dated inside the period", got.Resources)
		}
	})

	t.Run("refuses a period without a finalized run", func(t *testing.T) {
		completedFrom, completedTo := month(-13)
		f.seedRun(t, completedFrom, completedTo, "completed")
		unknownFrom, unknownTo := month(-14)

		for _, tc := range []struct {
			name     string
			from, to time.Time
		}{
			{name: "a completed run nothing finalized", from: completedFrom, to: completedTo},
			{name: "no run at all", from: unknownFrom, to: unknownTo},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := f.detectLate(t, tc.from, tc.to)
				if !errors.Is(err, runs.ErrPeriodNotFinalized) {
					t.Fatalf("DetectLate() error = %v, want one wrapping ErrPeriodNotFinalized", err)
				}
				if want := period.Format(tc.from); !strings.Contains(err.Error(), want) {
					t.Errorf("DetectLate() error = %q, want the month %s named", err, want)
				}
				if !reflect.DeepEqual(got, runs.LateReport{}) {
					t.Errorf("LateReport = %+v, want the zero report beside the refusal", got)
				}
			})
		}
	})

	t.Run("refuses a finalized run without a snapshot", func(t *testing.T) {
		from, to := month(-15)
		finalized := f.seedFinalizedPeriod(t, from, to, "v1")

		got, err := f.detectLate(t, from, to)
		if err == nil {
			t.Fatal("DetectLate() error = nil, want the missing snapshot reported")
		}
		for _, want := range []string{finalized.String(), "snapshot_at"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("DetectLate() error = %q, want %q named", err, want)
			}
		}
		if !reflect.DeepEqual(got, runs.LateReport{}) {
			t.Errorf("LateReport = %+v, want the zero report beside the refusal", got)
		}
	})
}

// TestDetectLateFollowsTheChain reports one period twice, before and after a
// correction of it is finalized: what the late events are held against moves
// from the regular run that closed the month to the correction that booked
// them. It runs on databases of its own, because the case bills its month twice
// and the correction supersedes what the period holds.
func TestDetectLateFollowsTheChain(t *testing.T) {
	f := newFixtureWith(t, correctionPricingDocument)
	from, to := month(-12)
	const cloud = "os-late-chain"
	f.seedProject(t, cloud, "proj-chain")
	vm := instance(cloud, "i-chain", "proj-chain", from, standardSize)
	f.seedResource(t, vm)

	regular := f.closedMonth(t, from, to, cloud)

	f.seedEvent(t, vm, "ev-off-i-chain", "compute.instance.power_off.end",
		vm.created.Add(12*time.Hour), `{"state":"shutoff"}`)
	f.seedEvent(t, vm, "ev-on-i-chain", "compute.instance.power_on.end",
		vm.created.Add(24*time.Hour), `{"state":"active"}`)

	before, err := f.detectLate(t, from, to)
	if err != nil {
		t.Fatalf("DetectLate() error = %v, want nil", err)
	}
	if before.RunID != regular {
		t.Errorf("RunID = %s, want the regular run %s that closed the month", before.RunID, regular)
	}
	if before.Kind != runs.KindRegular {
		t.Errorf("Kind = %q, want %q", before.Kind, runs.KindRegular)
	}
	if len(before.Resources) != 1 {
		t.Fatalf("Resources = %+v, want the one resource the power cycle belongs to", before.Resources)
	}

	correction, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
	if err != nil {
		t.Fatalf("Correct() error = %v, want nil", err)
	}
	if _, err := f.finalize(t, from, correction.RunID); err != nil {
		t.Fatalf("Finalize() error = %v, want nil", err)
	}

	after, err := f.detectLate(t, from, to)
	if err != nil {
		t.Fatalf("DetectLate() error = %v, want nil", err)
	}
	if after.RunID != correction.RunID {
		t.Errorf("RunID = %s, want the finalized correction %s", after.RunID, correction.RunID)
	}
	if after.Kind != runs.KindCorrection {
		t.Errorf("Kind = %q, want %q", after.Kind, runs.KindCorrection)
	}
	if want := f.storedSnapshotAt(t, correction.RunID); !after.SnapshotAt.Equal(want) {
		t.Errorf("SnapshotAt = %s, want the %s the correction stored", after.SnapshotAt, want)
	}
	if len(after.Resources) != 0 {
		t.Errorf("Resources = %+v, want none: the correction read the power cycle and booked it",
			after.Resources)
	}
}

// TestDetectLateReportsAnUpstreamFailure breaks the reporting database under a
// report. It runs on databases of its own, because what it does to the
// reporting one leaves nothing else able to read events.
func TestDetectLateReportsAnUpstreamFailure(t *testing.T) {
	f := newFixture(t)
	from, to := month(-16)
	const cloud = "os-late-upstream"
	f.seedProject(t, cloud, "proj-upstream")
	f.seedResource(t, instance(cloud, "i-upstream", "proj-upstream", from, standardSize))
	f.closedMonth(t, from, to, cloud)

	if _, err := f.reporting.Store.Pool().Exec(t.Context(), `DROP TABLE events`); err != nil {
		t.Fatalf("dropping the events table: %v", err)
	}

	_, err := f.detectLate(t, from, to)
	if err == nil {
		t.Fatal("DetectLate() error = nil, want the failed read reported")
	}
	if want := "listing the late events"; !strings.Contains(err.Error(), want) {
		t.Errorf("DetectLate() error = %q, want the read that failed named with %q", err, want)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("DetectLate() error = %v, want the database's own error wrapped", err)
	}
}
