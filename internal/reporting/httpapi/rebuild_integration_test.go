package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// rebuildRoute is the route the other Tally components call with the shared
// internal token.
const rebuildRoute = "/internal/projection/rebuild"

// TestRebuildProjectionOverHTTP drives POST /internal/projection/rebuild
// against projection rows that have drifted from the history they are derived
// from. Corrupting the rows by hand is what a rebuild is for: whatever a row
// holds, replaying the events has to produce it again.
func TestRebuildProjectionOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	t.Run("replays every resource when the body names no filter", func(t *testing.T) {
		const cloud = "os-http-rebuild-all"
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		ingestFleet(t, a, token, cloud, "vol-all-1", "vol-all-2", "vol-all-3")
		before := resourceRows(t, a, cloud)

		corrupt(t, a, cloud)

		got := rebuildResultOf(t, a.call(t, http.MethodPost, rebuildRoute, internalToken, []byte(`{}`)))

		if want := resourceKeys(t, a, ""); got.Rebuilt != want {
			t.Errorf("rebuilt = %d, want the %d resource keys the events table holds", got.Rebuilt, want)
		}
		if after := resourceRows(t, a, cloud); !reflect.DeepEqual(after, before) {
			t.Errorf("projection rows = %v, want them back at %v", after, before)
		}
	})

	t.Run("replays one cloud and leaves the others corrupted", func(t *testing.T) {
		const (
			healed  = "os-http-rebuild-healed"
			ignored = "os-http-rebuild-ignored"
		)
		_, healedToken := seedIngestCredential(t, a.queries, fixturePlatform, healed)
		_, ignoredToken := seedIngestCredential(t, a.queries, fixturePlatform, ignored)
		ingestFleet(t, a, healedToken, healed, "vol-healed-1", "vol-healed-2")
		ingestFleet(t, a, ignoredToken, ignored, "vol-ignored-1")
		before := resourceRows(t, a, healed)

		corrupt(t, a, healed, ignored)
		stillCorrupt := resourceRows(t, a, ignored)

		body := fmt.Appendf(nil, `{"cloud": %q}`, healed)
		got := rebuildResultOf(t, a.call(t, http.MethodPost, rebuildRoute, internalToken, body))

		if want := resourceKeys(t, a, healed); got.Rebuilt != want {
			t.Errorf("rebuilt = %d, want the %d resource keys of %s", got.Rebuilt, want, healed)
		}
		if after := resourceRows(t, a, healed); !reflect.DeepEqual(after, before) {
			t.Errorf("projection rows of %s = %v, want them back at %v", healed, after, before)
		}
		if after := resourceRows(t, a, ignored); !reflect.DeepEqual(after, stillCorrupt) {
			t.Errorf("projection rows of %s = %v, want the filter to have left them at %v",
				ignored, after, stillCorrupt)
		}
	})

	t.Run("rebuilds nothing for a filter no event matches", func(t *testing.T) {
		body := []byte(`{"cloud": "os-http-rebuild-nowhere"}`)

		got := rebuildResultOf(t, a.call(t, http.MethodPost, rebuildRoute, internalToken, body))

		if got.Rebuilt != 0 {
			t.Errorf("rebuilt = %d, want 0 for a filter no event matches", got.Rebuilt)
		}
	})

	t.Run("refuses a request that does not carry the internal token", func(t *testing.T) {
		for name, token := range map[string]string{
			"no token":      "",
			"a wrong token": internalToken + "-not",
		} {
			t.Run(name, func(t *testing.T) {
				rec := a.call(t, http.MethodPost, rebuildRoute, token, []byte(`{}`))

				assertChallenged(t, rec)
			})
		}
	})

	t.Run("records one audit row per call", func(t *testing.T) {
		const cloud = "os-http-rebuild-audited"
		before := len(auditRows(t, a, "internal", "current_resources.rebuild"))

		body := fmt.Appendf(nil, `{"cloud": %q}`, cloud)
		got := rebuildResultOf(t, a.call(t, http.MethodPost, rebuildRoute, internalToken, body))

		rows := auditRows(t, a, "internal", "current_resources.rebuild")
		if len(rows) != before+1 {
			t.Fatalf("rebuild audit rows = %d, want one more than the %d before the call", len(rows), before)
		}
		written := rows[len(rows)-1]
		if written.objectType != "current_resources" {
			t.Errorf("audit object type = %q, want %q", written.objectType, "current_resources")
		}
		want := map[string]any{
			"cloud":         cloud,
			"resource_type": "",
			"rebuilt":       float64(got.Rebuilt),
		}
		if !reflect.DeepEqual(written.details, want) {
			t.Errorf("audit details = %v, want %v", written.details, want)
		}
	})
}

// ingestFleet stores a create and a resize event per resource through the API,
// which is the history a rebuild replays.
func ingestFleet(t *testing.T, a api, token, cloud string, resourceIDs ...string) {
	t.Helper()

	var items []json.RawMessage
	for _, id := range resourceIDs {
		res := fixture{cloud: cloud, resourceType: "volume", id: id}
		items = append(items,
			item(t, res.event(id+"-create", "volume.create", createTime,
				payloadOf("available", volumeSize(10)))),
			item(t, res.event(id+"-resize", "volume.resize", updateTime,
				payloadOf("in-use", volumeSize(20)))))
	}

	got := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, batch(t, items...)))
	if got.Accepted != len(items) {
		t.Fatalf("result = %+v, want all %d events of %s accepted", got, len(items), cloud)
	}
}

// corrupt overwrites the state of every projection row of the given clouds,
// which is the drift a rebuild has to heal.
func corrupt(t *testing.T, a api, clouds ...string) {
	t.Helper()

	tag, err := a.store.Pool().Exec(t.Context(),
		`UPDATE current_resources SET state = 'corrupt' WHERE cloud = ANY($1)`, clouds)
	if err != nil {
		t.Fatalf("corrupting the projection rows of %v: %v", clouds, err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatalf("corrupting the projection rows of %v changed nothing, want the rows to exist", clouds)
	}
}

// resourceKeys is how many distinct resources the events table holds for the
// cloud, which is what a rebuild of that cloud reports as rebuilt. An empty
// cloud counts every key, the way a request without a filter selects them.
func resourceKeys(t *testing.T, a api, cloud string) int {
	t.Helper()

	var found int
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM (
		     SELECT DISTINCT cloud, resource_type, resource_id FROM events
		     WHERE $1::text = '' OR cloud = $1::text
		 ) keys`, cloud).Scan(&found); err != nil {
		t.Fatalf("counting the resource keys of %q: %v", cloud, err)
	}
	return found
}

// rebuildResultOf decodes the answer of a rebuild, which the contract promises
// is a 200 carrying how many resources were replayed.
func rebuildResultOf(t *testing.T, rec *httptest.ResponseRecorder) RebuildResult {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}

	var got RebuildResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	return got
}
