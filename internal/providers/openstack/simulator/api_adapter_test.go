package simulator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/reconciliation/adapters"
)

// The round trip of the two halves a drill is made of. The fake API serves a
// generated month, and the reconciliation adapter of the Reporting API reads it
// back over the wire, through the clouds.yaml every other OpenStack client
// reads. What the two have to agree on is one statement: a resource nothing
// drifted on is observed exactly as the oracle's interval states it, down to
// the size object, so the corrections a told sync writes are the ones the fault
// switches earned and nothing else. An observation that disagreed here would
// have the drill report drift the engine never caused.
//
// The adapter is the real one, and this is the only direction the import runs
// in: a test of the simulator reads the Reporting API's adapter, and nothing in
// the Reporting API knows the simulator exists.
//
// Nothing here is fixed by hand. The month is generated, the resources are
// whatever the seed drew, and the assertions hold every one of them against the
// oracle, so a listing that lost a member or a conversion that lost a digit
// fails on the resources it touched rather than on the ones a fixture happened
// to name.

// syncWindow is how far back the run asks the cloud for the instances it
// destroyed. It stays inside the day the adapter clamps such a window to
// (maxDeletedWindow in adapters/openstack.go): a longer one is cut to that day,
// the deletes before the cut are never asked for, and this test would be about
// the clamp rather than about the round trip.
const syncWindow = 12 * time.Hour

// enumeration is what one run of the adapter made of the month: the resources
// it observed, the errors it reported instead, and how often it asked for the
// flavor catalog.
type enumeration struct {
	resources      []reconciliation.ObservedResource
	errs           []error
	flavorListings int
}

// partition splits what a run observed into the resources the cloud holds and
// the ones it reported as deleted, each keyed the way the oracle keys a
// resource. The two halves are asserted on separately, because a deleted
// resource is reported by its key and the instant alone: what it was, how big
// it was and who owned it is what the projection row already holds.
func partition(observed []reconciliation.ObservedResource,
) (live, gone map[resourceKey]reconciliation.ObservedResource) {
	live = make(map[resourceKey]reconciliation.ObservedResource)
	gone = make(map[resourceKey]reconciliation.ObservedResource)
	for _, resource := range observed {
		key := resourceKey{resourceType: resource.ResourceType, resourceID: resource.ResourceID}
		if resource.DeletedAt != nil {
			gone[key] = resource
			continue
		}
		live[key] = resource
	}
	return live, gone
}

// syncMonth is the month both round trips read. pre-existing puts resources
// into it that were there before it began, whose create no collector of the
// month can have recorded, and held-back keeps transitions off the bus, which
// is the drift a drill's sync is there to correct. missing-create is not among
// them: it excludes pre-existing (ParseFaults), and what this test reads is the
// created instant of a pre-existing resource.
func syncMonth(t *testing.T) Month {
	t.Helper()

	faults, err := ParseFaults([]string{FaultPreExisting, FaultHeldBack})
	if err != nil {
		t.Fatalf("ParseFaults() error = %v, want nil", err)
	}
	return faultyMonth(t, 1, faults)
}

// observeMonth serves the oracle at the instant at and runs the real adapter
// against it. The clock is frozen there, so the inventory a listing answers
// with is the one of at however long the run takes.
func observeMonth(t *testing.T, oracle Oracle, at, since time.Time) enumeration {
	t.Helper()

	api, err := NewCloudAPI(NewClock(at, 0, time.Now), oracle)
	if err != nil {
		t.Fatalf("NewCloudAPI() error = %v, want nil", err)
	}

	var flavorListings atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == flavorsPath {
			flavorListings.Add(1)
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	writeSyncCloudsYAML(t, server.URL)

	var run enumeration
	stream := adapters.NewOpenStack(slog.New(slog.DiscardHandler)).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud, "include_octavia": true}, &since, at)
	for resource, err := range stream {
		if err != nil {
			run.errs = append(run.errs, err)
			continue
		}
		run.resources = append(run.resources, resource)
	}
	run.flavorListings = int(flavorListings.Load())
	return run
}

// writeSyncCloudsYAML points the process at a clouds.yaml that authenticates
// against the fake API, with the credentials the simulated keystone accepts.
// OS_CLIENT_CONFIG_FILE makes the written file the only search location, so the
// adapter runs its production lookup and still cannot reach a developer's real
// clouds.yaml.
//
// The other OS_* variables are emptied for the same reason: they override the
// file, and a shell that has them set from a real cloud would otherwise decide
// what the test authenticates as.
//
// Setting the environment is process-wide, which is why neither test here runs
// in parallel.
func writeSyncCloudsYAML(t *testing.T, serverURL string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "clouds.yaml")
	content := fmt.Sprintf(`clouds:
  %s:
    auth:
      auth_url: %s/v3
      username: %s
      password: %s
      project_id: %s
      user_domain_name: Default
    region_name: %s
    interface: public
`, testCloud, serverURL, cloudUsername, cloudPassword, cloudProjectID, cloudRegion)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("OS_CLIENT_CONFIG_FILE", path)
	for _, name := range []string{"OS_CACERT", "OS_CERT", "OS_INTERFACE", "OS_KEY", "OS_REGION_NAME"} {
		t.Setenv(name, "")
	}
}

// statedAt is the interval the oracle states a resource was in at at, and false
// where it states none: one whose first interval has not started yet, and one
// whose last interval has ended. The intervals are half-open.
//
// The oracle is read here rather than through the API's own liveAt, so that the
// two sides of the round trip stay two statements: a fake that read its
// inventory out of the wrong end of an interval would otherwise be held against
// its own reading.
func statedAt(resource OracleResource, at time.Time) (OracleInterval, bool) {
	for _, interval := range resource.Intervals {
		if !at.Before(interval.From) && at.Before(interval.To) {
			return interval, true
		}
	}
	return OracleInterval{}, false
}

// endedBy is when the oracle states a resource was deleted, as that stands at
// at, and false for one that is still there or was never created. A last
// interval that ends before the month does is a delete; one that ends with the
// month is a resource that outlived it.
func endedBy(resource OracleResource, at, periodTo time.Time) (time.Time, bool) {
	end := resource.Intervals[len(resource.Intervals)-1].To
	if end.Before(periodTo) && !at.Before(end) {
		return end, true
	}
	return time.Time{}, false
}

// sizeText renders a size object the way the projection stores and the diff
// compares one: as the JSON document it is marshaled to. That is the comparison
// the sync runs (sameSize in reconciliation/sync.go), and it is the one that
// matters here: the adapter builds a count as a Go int where the oracle holds a
// json.Number, and the two are one size rather than drift, while a quarter
// gibibyte that took a detour through a float would read differently.
func sizeText(t *testing.T, size map[string]any) string {
	t.Helper()

	if size == nil {
		return "none"
	}
	encoded, err := json.Marshal(size)
	if err != nil {
		t.Fatalf("Marshal(%#v): %v", size, err)
	}
	return string(encoded)
}

// TestFakeAPIObservesThroughTheRealAdapterWhatTheOracleStates is the whole of
// what the fake API is for. Every resource the oracle states at the instant the
// clock stands at is observed once, with the state, the project and the size
// the oracle states, and nothing else is observed at all.
func TestFakeAPIObservesThroughTheRealAdapterWhatTheOracleStates(t *testing.T) {
	oracle := syncMonth(t).Oracle
	// Twenty days into the month, so that the cloud holds what it holds because
	// of the intervals rather than because the month has barely started.
	at := oracle.PeriodFrom.AddDate(0, 0, 20)
	since := at.Add(-syncWindow)

	statedLive := make(map[resourceKey]OracleInterval)
	statedGone := make(map[resourceKey]time.Time)
	var alreadyGone, notYetCreated int
	for _, resource := range oracle.Resources {
		key := resourceKey{resourceType: resource.ResourceType, resourceID: resource.ResourceID}
		if interval, ok := statedAt(resource, at); ok {
			statedLive[key] = interval
			continue
		}
		end, deleted := endedBy(resource, at, oracle.PeriodTo)
		if !deleted {
			notYetCreated++
			continue
		}
		alreadyGone++
		// Nova is the one of the five services that names its deletions, and it
		// names the ones inside the window the run carries, so the instances of
		// that window are the whole of what a run observes as deleted.
		if key.resourceType == typeInstance && end.After(since) {
			statedGone[key] = end
		}
	}

	// What the instant has to be worth asserting at. A month whose resources
	// were all still to come, or all already gone, would pass every assertion
	// below while proving nothing about either.
	switch {
	case len(statedLive) == 0:
		t.Fatalf("the oracle holds nothing at %s, so there is nothing to observe", instantText(at))
	case alreadyGone == 0:
		t.Fatalf("the oracle deletes nothing before %s, so nothing holds the cloud to "+
			"leaving out what is gone", instantText(at))
	case notYetCreated == 0:
		t.Fatalf("the oracle creates nothing after %s, so nothing holds the cloud to "+
			"leaving out what has not happened yet", instantText(at))
	case len(statedGone) == 0:
		t.Fatalf("the oracle deletes no instance inside %s, so nothing holds the deleted "+
			"listing to the window", boundsText(since, at))
	}

	run := observeMonth(t, oracle, at, since)
	if len(run.errs) != 0 {
		t.Fatalf("the enumeration reported %v, want no error", run.errs)
	}

	// A resource observed twice would be one key here, which is what the count
	// catches: the diff books one of the two observations, and nothing would say
	// the other one arrived.
	live, gone := partition(run.resources)
	if observed := len(live) + len(gone); observed != len(run.resources) {
		t.Errorf("the run observed %d resources under %d keys, want each of them once",
			len(run.resources), observed)
	}

	for key, interval := range statedLive {
		observed, ok := live[key]
		if !ok {
			t.Errorf("%s %s is not observed, and the oracle states it live at %s",
				key.resourceType, key.resourceID, instantText(at))
			continue
		}
		if observed.State != interval.State {
			t.Errorf("%s %s is observed as %q, want the %q the oracle states",
				key.resourceType, key.resourceID, observed.State, interval.State)
		}
		if observed.ProjectID != interval.ProjectID {
			t.Errorf("%s %s is observed under the project %q, want the %q the oracle states",
				key.resourceType, key.resourceID, observed.ProjectID, interval.ProjectID)
		}
		if got, want := sizeText(t, observed.Size), sizeText(t, interval.Size); got != want {
			t.Errorf("%s %s is observed as %s, want the %s the oracle states",
				key.resourceType, key.resourceID, got, want)
		}
	}
	for key := range live {
		if _, ok := statedLive[key]; !ok {
			t.Errorf("%s %s is observed, and the oracle states no interval of it that holds %s",
				key.resourceType, key.resourceID, instantText(at))
		}
	}

	// A delete the platform named is the one correction a sync can date at the
	// instant it happened, so the instants are held to the oracle's own rather
	// than to the window alone.
	for key, end := range statedGone {
		observed, ok := gone[key]
		if !ok {
			t.Errorf("%s %s is not observed as deleted, and the oracle deletes it at %s, "+
				"inside %s", key.resourceType, key.resourceID, instantText(end), boundsText(since, at))
			continue
		}
		if !observed.DeletedAt.Equal(end) {
			t.Errorf("%s %s is observed as deleted at %s, want the %s the oracle states",
				key.resourceType, key.resourceID, instantText(*observed.DeletedAt), instantText(end))
		}
	}
	for key, observed := range gone {
		if _, ok := statedGone[key]; !ok {
			t.Errorf("%s %s is observed as deleted at %s, and the oracle states no delete of it "+
				"inside %s", key.resourceType, key.resourceID,
				instantText(*observed.DeletedAt), boundsText(since, at))
		}
	}

	// A resource that was there before the month began is observed as created at
	// the month's first instant, which is what lets a told sync date the create
	// nobody published rather than book it at poll time. One the month has since
	// deleted carries no observation to read that instant off, so the ones the
	// cloud still holds are what this is asserted over.
	var preExisting int
	for _, resource := range oracle.Resources {
		key := resourceKey{resourceType: resource.ResourceType, resourceID: resource.ResourceID}
		observed, ok := live[key]
		if !ok || !slices.Contains(resource.Faults, FaultPreExisting) {
			continue
		}
		preExisting++
		if !resource.Intervals[0].From.Equal(oracle.PeriodFrom) {
			t.Errorf("the oracle starts the pre-existing %s %s at %s, want the month's first "+
				"instant %s", key.resourceType, key.resourceID,
				instantText(resource.Intervals[0].From), instantText(oracle.PeriodFrom))
		}
		if observed.CreatedAt == nil {
			t.Errorf("%s %s is observed without a created instant, which is what a missed "+
				"create is dated by", key.resourceType, key.resourceID)
			continue
		}
		if !observed.CreatedAt.Equal(oracle.PeriodFrom) {
			t.Errorf("the pre-existing %s %s is observed as created at %s, want the month's "+
				"first instant %s", key.resourceType, key.resourceID,
				instantText(*observed.CreatedAt), instantText(oracle.PeriodFrom))
		}
	}
	if preExisting == 0 {
		t.Error("the month holds no resource the pre-existing switch put before it, " +
			"so nothing holds an observed create to the month's first instant")
	}

	// The flavor catalog is the fallback of a reader that could not negotiate
	// the microversion which embeds a server's flavor. A run that read it
	// negotiated nothing, and an instance on a flavor the catalog left out would
	// then be observed without a size.
	if run.flavorListings != 0 {
		t.Errorf("the run read the flavor catalog %d times, want none: the microversion the "+
			"adapter negotiates carries a server's flavor in the server", run.flavorListings)
	}
}

// TestFakeAPIObservesNothingThroughTheRealAdapterBeforeTheMonthBegins covers
// the cloud that holds nothing yet. Every interval of an oracle starts inside
// the month, so a clock standing before it stands before every one of them, and
// an empty cloud has to read as an empty cloud rather than as a failure: a sync
// that took it for one would leave every row of the projection alone.
func TestFakeAPIObservesNothingThroughTheRealAdapterBeforeTheMonthBegins(t *testing.T) {
	oracle := syncMonth(t).Oracle
	at := oracle.PeriodFrom.Add(-time.Hour)

	run := observeMonth(t, oracle, at, at.Add(-syncWindow))
	if len(run.errs) != 0 {
		t.Fatalf("the enumeration reported %v, want no error", run.errs)
	}
	for _, observed := range run.resources {
		t.Errorf("%s %s is observed at %s, an hour before the month the oracle states begins",
			observed.ResourceType, observed.ResourceID, instantText(at))
	}
}
