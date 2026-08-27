package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/projects"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// projectsRoute is the route the registry is listed and written under, spelled
// once because every subtest addresses it.
const projectsRoute = "/api/v1/projects"

// TestProjectsOverHTTP drives the four registry operations through the whole
// stack: the contract validator, the role a route demands, the transactions
// behind the writes, and the rows they leave. The subtests share one database
// and work on clouds of their own, so a project one of them registers stays out
// of what another asserts about a list.
func TestProjectsOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	adminID, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)
	_, projectToken := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{uuid.New()})

	t.Run("registers a project and answers it under the id that reads it back", func(t *testing.T) {
		const cloud = "os-reg-create"

		rec := a.call(t, http.MethodPost, projectsRoute, adminToken, projectDocument(t, map[string]any{
			"platform": fixturePlatform, "cloud": cloud, "external_id": "reg-create",
			"name": "The first project",
		}))

		created := registeredProject(t, rec)
		if got, want := rec.Header().Get("Location"), projectPath(created.Id); got != want {
			t.Errorf("Location = %q, want %q", got, want)
		}
		if created.Name == nil || *created.Name != "The first project" {
			t.Errorf("name = %v, want the registered one", created.Name)
		}
		// A registration that carries no metadata registers the empty object,
		// which is what the contract types the member as.
		if len(created.Metadata) != 0 {
			t.Errorf("metadata = %v, want the empty object", created.Metadata)
		}
		// The instant is read off the wire rather than off the model: time.Time
		// has parsed the zone away by the time the model carries it.
		if at := memberOnTheWire(t, rec, "created_at"); !strings.HasSuffix(at, "Z") {
			t.Errorf("created_at = %q, want it stated in UTC", at)
		}

		// The registration and the read answer the same bytes, which is what says
		// the two renderers agree rather than merely both being valid.
		read := a.call(t, http.MethodGet, projectPath(created.Id), adminToken, nil)
		if got := read.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, read.Body)
		}
		if got, want := read.Body.String(), rec.Body.String(); got != want {
			t.Errorf("the read answered %s, want the registration's %s", got, want)
		}

		rows := projectAudits(t, a, adminID.String(), projects.ActionCreate, created.Id)
		if len(rows) != 1 {
			t.Fatalf("registration audit rows = %v, want exactly one", rows)
		}
		if got := rows[0].objectType; got != auditObjectProjects {
			t.Errorf("audit object type = %q, want %q", got, auditObjectProjects)
		}
	})

	t.Run("refuses a second registration of one cloud and external id", func(t *testing.T) {
		const (
			cloud      = "os-reg-conflict"
			externalID = "reg-conflict"
		)
		first := registerProject(t, a, adminToken, fixturePlatform, cloud, externalID)

		rec := a.call(t, http.MethodPost, projectsRoute, adminToken, projectDocument(t, map[string]any{
			"platform": fixturePlatform, "cloud": cloud, "external_id": externalID,
			"name": "The second registration of one pair",
		}))

		assertProblem(t, rec, http.StatusConflict, problem.TypeConflict)
		if got, want := problemDetail(t, rec),
			"a project with this cloud and external id is already registered"; got != want {
			t.Errorf("detail = %q, want %q", got, want)
		}
		// The refusal leaves the registered project as it was rather than
		// replacing its name with the one the second call carried.
		got := projectFrom(t, a.call(t, http.MethodGet, projectPath(first.Id), adminToken, nil))
		if !reflect.DeepEqual(got, first) {
			t.Errorf("the registered project = %+v, want the unchanged %+v", got, first)
		}

		// The key is the pair, so the same external id under another cloud is a
		// project of its own rather than a duplicate.
		second := registerProject(t, a, adminToken, fixturePlatform, cloud+"-b", externalID)
		if second.Id == first.Id {
			t.Errorf("the second cloud reused the row %s, want a project of its own", first.Id)
		}
	})

	t.Run("holds a virtual platform to its own cloud", func(t *testing.T) {
		// The refusals run before the two registrations that get through, so a
		// cloud that lists nothing afterwards is the refusal having written
		// nothing rather than a project nobody has registered yet.
		audits := len(auditRows(t, a, adminID.String(), projects.ActionCreate))

		for _, tc := range []struct {
			name       string
			platform   string
			cloud      string
			externalID string
		}{
			{
				name:     "a meta project under a cloud of its own",
				platform: "meta", cloud: "os-virtual", externalID: "virtual-key-1",
			},
			{
				name:     "a real platform under a virtual cloud",
				platform: fixturePlatform, cloud: "meta", externalID: "virtual-key-2",
			},
			{
				name:     "one virtual platform under the other",
				platform: "meta", cloud: "partner", externalID: "virtual-key-3",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodPost, projectsRoute, adminToken, projectDocument(t,
					map[string]any{
						"platform": tc.platform, "cloud": tc.cloud, "external_id": tc.externalID,
					}))

				assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeValidation)
				if got := problemDetail(t, rec); got != virtualKeyDetail {
					t.Errorf("detail = %q, want %q", got, virtualKeyDetail)
				}
				// The cloud is what the caller has to change, whichever end of the
				// pair names the virtual platform.
				if got, want := fieldErrorLocations(t, rec), []string{"body.cloud"}; !slices.Equal(got, want) {
					t.Errorf("field errors = %v, want %v", got, want)
				}

				list := projectListOf(t, a.call(t, http.MethodGet,
					projectsRoute+"?cloud="+tc.cloud, adminToken, nil))
				if len(list.Items) != 0 {
					t.Errorf("the cloud %s holds %v, want the refusal to have written nothing",
						tc.cloud, projectKeys(list.Items))
				}
			})
		}

		if got := len(auditRows(t, a, adminID.String(), projects.ActionCreate)); got != audits {
			t.Errorf("registration audit rows = %d, want the %d the refusals started with",
				got, audits)
		}

		meta := registeredProject(t, a.call(t, http.MethodPost, projectsRoute, adminToken,
			projectDocument(t, map[string]any{
				"platform": "meta", "cloud": "meta", "external_id": "customer-alpha",
				"name": "Customer Alpha",
			})))
		if meta.Platform != "meta" || meta.Cloud != "meta" {
			t.Errorf("the meta-project = (%q, %q), want it carrying its platform as its cloud",
				meta.Platform, meta.Cloud)
		}

		partner := registeredProject(t, a.call(t, http.MethodPost, projectsRoute, adminToken,
			projectDocument(t, map[string]any{
				"platform": "partner", "cloud": "partner", "external_id": "partner-corp",
			})))
		if partner.Platform != "partner" || partner.Cloud != "partner" {
			t.Errorf("the partner = (%q, %q), want it carrying its platform as its cloud",
				partner.Platform, partner.Cloud)
		}

		// A virtual project is keyed like every other one, so the pair it
		// registered under registers once.
		rec := a.call(t, http.MethodPost, projectsRoute, adminToken, projectDocument(t,
			map[string]any{
				"platform": "meta", "cloud": "meta", "external_id": "customer-alpha",
			}))

		assertProblem(t, rec, http.StatusConflict, problem.TypeConflict)
		if got, want := problemDetail(t, rec),
			"a project with this cloud and external id is already registered"; got != want {
			t.Errorf("detail = %q, want %q", got, want)
		}
	})

	t.Run("lists the registry", func(t *testing.T) {
		const (
			cloudA   = "os-reg-list-a"
			cloudB   = "os-reg-list-b"
			platform = "tallytest-list"
		)
		// Registered out of order, so what sorts the answer is the query rather
		// than the order the rows were written in.
		registerProject(t, a, adminToken, fixturePlatform, cloudB, "reg-list-2")
		registerProject(t, a, adminToken, fixturePlatform, cloudA, "reg-list-3")
		registerProject(t, a, adminToken, fixturePlatform, cloudA, "reg-list-1")
		registerProject(t, a, adminToken, platform, cloudA, "reg-list-2")
		registerProject(t, a, adminToken, fixturePlatform, cloudB, "reg-list-1")

		t.Run("narrows it by the filters the request carries", func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				query string
				want  []string
			}{
				{
					name:  "by platform",
					query: "platform=" + platform,
					want:  []string{cloudA + ":reg-list-2"},
				},
				{
					name:  "by cloud, ordered by external id",
					query: "cloud=" + cloudA,
					want: []string{
						cloudA + ":reg-list-1", cloudA + ":reg-list-2", cloudA + ":reg-list-3",
					},
				},
				{
					// An external id is not a key on its own: two clouds naming a
					// project the same way are two projects, and both are served.
					name:  "by external id, which two clouds share",
					query: "external_id=reg-list-1",
					want:  []string{cloudA + ":reg-list-1", cloudB + ":reg-list-1"},
				},
				{
					name:  "by the pair the registry is keyed by",
					query: "cloud=" + cloudB + "&external_id=reg-list-2",
					want:  []string{cloudB + ":reg-list-2"},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					list := projectListOf(t, a.call(t, http.MethodGet,
						projectsRoute+"?"+tc.query, adminToken, nil))

					if got := projectKeys(list.Items); !slices.Equal(got, tc.want) {
						t.Errorf("served projects = %v, want %v", got, tc.want)
					}
					if list.NextCursor != nil {
						t.Errorf("next_cursor = %q, want null on a page that holds everything",
							*list.NextCursor)
					}
				})
			}
		})

		t.Run("walks a filtered list page by page", func(t *testing.T) {
			whole := projectListOf(t, a.call(t, http.MethodGet,
				projectsRoute+"?cloud="+cloudA, adminToken, nil))
			if len(whole.Items) != 3 {
				t.Fatalf("the unpaginated answer holds %d projects, want 3", len(whole.Items))
			}

			for _, tc := range []struct {
				name  string
				limit int
				sizes []int
			}{
				{name: "a page per project", limit: 1, sizes: []int{1, 1, 1}},
				{name: "a last page shorter than the limit", limit: 2, sizes: []int{2, 1}},
				// The whole list in one page: the last page is exactly full, which
				// is what tells "the page is full" from "there is more".
				{name: "a last page exactly as long as the limit", limit: 3, sizes: []int{3}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					page := fmt.Sprintf("%s?cloud=%s&limit=%d", projectsRoute, cloudA, tc.limit)

					var walked []Project
					var sizes []int
					for count := 1; ; count++ {
						if count > len(whole.Items) {
							t.Fatalf("the walk had not ended after %d pages, want it over after %d",
								count, len(tc.sizes))
						}
						list := projectListOf(t, a.call(t, http.MethodGet, page, adminToken, nil))

						sizes = append(sizes, len(list.Items))
						walked = append(walked, list.Items...)
						if list.NextCursor == nil {
							break
						}
						page = fmt.Sprintf("%s?cloud=%s&limit=%d&cursor=%s",
							projectsRoute, cloudA, tc.limit, url.QueryEscape(*list.NextCursor))
					}

					if !slices.Equal(sizes, tc.sizes) {
						t.Errorf("page sizes = %v, want %v", sizes, tc.sizes)
					}
					if !reflect.DeepEqual(walked, whole.Items) {
						t.Errorf("the walk served %v, want the unpaginated %v",
							projectKeys(walked), projectKeys(whole.Items))
					}
				})
			}
		})

		t.Run("answers a filter nothing matches with the empty page", func(t *testing.T) {
			rec := a.call(t, http.MethodGet, projectsRoute+"?cloud=os-reg-list-nothing",
				adminToken, nil)

			if got := rec.Code; got != http.StatusOK {
				t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
			}
			// The raw body rather than the decoded one: a client iterates items
			// without a nil check, which null would break and [] does not.
			want := `{"items":[],"next_cursor":null}`
			if got := strings.TrimSpace(rec.Body.String()); got != want {
				t.Errorf("body = %s, want %s", got, want)
			}
		})
	})

	t.Run("refuses a cursor it did not issue", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			cursor string
		}{
			{name: "not base64url", cursor: "not base64!"},
			{name: "one key where the sort key has two", cursor: encodeCursor([]string{"os-reg-list-a"})},
			// The sort key of the resource list has three parts, so a cursor of
			// that list resumes nothing here.
			{
				name:   "three keys where the sort key has two",
				cursor: encodeCursor([]string{"os-reg-list-a", "volume", "vol-1"}),
			},
			{name: "an empty key", cursor: encodeCursor([]string{"os-reg-list-a", ""})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet,
					projectsRoute+"?cursor="+url.QueryEscape(tc.cursor), adminToken, nil)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
				if got, want := problemDetail(t, rec), "the cursor is not one this API issued"; got != want {
					t.Errorf("detail = %q, want %q", got, want)
				}
			})
		}
	})

	t.Run("refuses a read of a project it cannot serve", func(t *testing.T) {
		t.Run("an id the registry does not hold", func(t *testing.T) {
			rec := a.call(t, http.MethodGet, projectPath(uuid.New()), adminToken, nil)

			assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
			if got, want := problemDetail(t, rec), "this project is not registered"; got != want {
				t.Errorf("detail = %q, want %q", got, want)
			}
		})

		t.Run("an id that is not a UUID", func(t *testing.T) {
			// The contract carries the RFC 4122 pattern next to the format, so the
			// request validator refuses the id before the route is even matched:
			// the answer is the contract's 400 rather than a failure of the
			// generated binding behind it.
			rec := a.call(t, http.MethodGet, projectsRoute+"/not-a-uuid", adminToken, nil)

			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			if got, want := fieldErrorLocations(t, rec), []string{"path.id"}; !slices.Equal(got, want) {
				t.Errorf("field errors = %v, want %v", got, want)
			}
		})
	})

	t.Run("updates a project", func(t *testing.T) {
		t.Run("changes the name and the metadata it names", func(t *testing.T) {
			project := registerProject(t, a, adminToken, fixturePlatform, "os-reg-update", "reg-update")
			metadata := map[string]any{"owner": "team-a", "tier": "gold"}

			updated := projectFrom(t, a.call(t, http.MethodPatch, projectPath(project.Id), adminToken,
				projectDocument(t, map[string]any{"name": "Renamed", "metadata": metadata})))

			if updated.Name == nil || *updated.Name != "Renamed" {
				t.Errorf("name = %v, want the updated one", updated.Name)
			}
			if !reflect.DeepEqual(updated.Metadata, metadata) {
				t.Errorf("metadata = %v, want %v", updated.Metadata, metadata)
			}
			// The key and the registration instant are not writable here, so they
			// come back as they were.
			if updated.Cloud != project.Cloud || updated.ExternalId != project.ExternalId ||
				!updated.CreatedAt.Equal(project.CreatedAt) {
				t.Errorf("the updated project = %+v, want the key and created_at of %+v",
					updated, project)
			}

			rows := projectAudits(t, a, adminID.String(), actionUpdateProject, project.Id)
			if len(rows) != 1 {
				t.Fatalf("update audit rows = %v, want exactly one", rows)
			}
			if got := rows[0].objectType; got != auditObjectProjects {
				t.Errorf("audit object type = %q, want %q", got, auditObjectProjects)
			}
		})

		t.Run("leaves the metadata of a request that names only the name", func(t *testing.T) {
			project := registerProject(t, a, adminToken, fixturePlatform, "os-reg-update-name", "reg-update-name")
			metadata := map[string]any{"owner": "team-b"}
			stored := projectFrom(t, a.call(t, http.MethodPatch, projectPath(project.Id), adminToken,
				projectDocument(t, map[string]any{"metadata": metadata})))
			if !reflect.DeepEqual(stored.Metadata, metadata) {
				t.Fatalf("metadata = %v, want the %v the setup wrote", stored.Metadata, metadata)
			}

			updated := projectFrom(t, a.call(t, http.MethodPatch, projectPath(project.Id), adminToken,
				projectDocument(t, map[string]any{"name": "Renamed once more"})))

			if updated.Name == nil || *updated.Name != "Renamed once more" {
				t.Errorf("name = %v, want the updated one", updated.Name)
			}
			if !reflect.DeepEqual(updated.Metadata, metadata) {
				t.Errorf("metadata = %v, want the untouched %v", updated.Metadata, metadata)
			}
		})

		t.Run("refuses an id the registry does not hold", func(t *testing.T) {
			rec := a.call(t, http.MethodPatch, projectPath(uuid.New()), adminToken,
				projectDocument(t, map[string]any{"name": "Renamed"}))

			assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
			if got, want := problemDetail(t, rec), "this project is not registered"; got != want {
				t.Errorf("detail = %q, want %q", got, want)
			}
		})

		t.Run("refuses a body that changes nothing", func(t *testing.T) {
			project := registerProject(t, a, adminToken, fixturePlatform, "os-reg-update-empty", "reg-update-empty")

			// The contract asks an update for at least one member, so the empty
			// object is refused rather than served as an update of nothing.
			rec := a.call(t, http.MethodPatch, projectPath(project.Id), adminToken, []byte(`{}`))

			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			if rows := projectAudits(t, a, adminID.String(), actionUpdateProject, project.Id); len(rows) != 0 {
				t.Errorf("update audit rows = %v, want none for a refused update", rows)
			}
		})
	})

	// The contract types metadata as a free object, so a member naming a number
	// the float64 range does not hold is a document it accepts. Every renderer
	// behind the contract reads the whole document, so a body the contract took
	// has to come back as an answer rather than as this service's own failure —
	// and it has to come back from every route that renders the row, because one
	// poisoned row would otherwise answer the whole registry 500.
	t.Run("keeps a metadata number no float64 holds", func(t *testing.T) {
		const cloud = "os-reg-huge"

		// The literal goes into the body as it stands, because no Go float64
		// could carry it there. The column is jsonb, which holds a number as a
		// numeric, so the answer spells the same value out rather than echoing
		// the exponent the request sent.
		const huge = "1e999"
		spelled := "1" + strings.Repeat("0", 999)

		// budgetOf is the number one answer carries under budget. The answer is
		// read member by member rather than decoded whole, because a decode into
		// map[string]any is the very thing that used to fail here.
		budgetOf := func(t *testing.T, rec *httptest.ResponseRecorder) string {
			t.Helper()

			var members map[string]json.RawMessage
			raw := memberRaw(t, rec, "metadata")
			if err := json.Unmarshal([]byte(raw), &members); err != nil {
				t.Fatalf("decoding the metadata %s: %v", raw, err)
			}
			return string(members["budget"])
		}

		rec := a.call(t, http.MethodPost, projectsRoute, adminToken, projectDocument(t, map[string]any{
			"platform": fixturePlatform, "cloud": cloud, "external_id": "reg-huge",
			"metadata": map[string]any{"budget": json.RawMessage(huge)},
		}))

		if got := rec.Code; got != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusCreated, rec.Body)
		}
		if got := budgetOf(t, rec); got != spelled {
			t.Errorf("budget = %s, want the sent %s spelled out", got, huge)
		}
		// Only the id is decoded off the answer: the model types metadata as a
		// map[string]any, so decoding the project whole would fail on the very
		// number this asserts about.
		var created struct {
			ID uuid.UUID `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decoding the body %q: %v", rec.Body, err)
		}

		read := a.call(t, http.MethodGet, projectPath(created.ID), adminToken, nil)
		if got := read.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, read.Body)
		}
		if got := budgetOf(t, read); got != spelled {
			t.Errorf("budget = %s, want the sent %s spelled out", got, huge)
		}

		// The list renders the same row, so a row it cannot render takes the
		// default listing of every caller down with it.
		list := a.call(t, http.MethodGet, projectsRoute+"?cloud="+cloud, adminToken, nil)
		if got := list.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, list.Body)
		}
		if got := list.Body.String(); !strings.Contains(got, spelled) {
			t.Errorf("the list answered %s, want it to carry the spelled out %s", got, huge)
		}
	})

	// The request cap of the router bounds what a body spells, not what the
	// column holds, and jsonb expands an exponent into every digit it names. A
	// document past the cap is therefore refused rather than stored for every
	// later read to answer.
	t.Run("refuses metadata the column expands past the cap", func(t *testing.T) {
		// Eighteen bytes in the body, 131072 digits in the column.
		const expanding = "1e131071"
		metadata := map[string]any{"budget": json.RawMessage(expanding)}

		rec := a.call(t, http.MethodPost, projectsRoute, adminToken, projectDocument(t, map[string]any{
			"platform": fixturePlatform, "cloud": "os-reg-cap", "external_id": "reg-cap",
			"metadata": metadata,
		}))

		assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeValidation)
		if got, want := fieldErrorLocations(t, rec), []string{"body.metadata"}; !slices.Equal(got, want) {
			t.Errorf("field errors = %v, want %v", got, want)
		}
		// The refusal rolls the registration back, so the cloud lists nothing.
		listed := projectListOf(t, a.call(t, http.MethodGet,
			projectsRoute+"?cloud=os-reg-cap", adminToken, nil))
		if len(listed.Items) != 0 {
			t.Errorf("the registry lists %v, want a refused registration to write none", listed.Items)
		}

		// An update carrying such a document is refused the same way, and the
		// project keeps what it had.
		project := registerProject(t, a, adminToken, fixturePlatform, "os-reg-cap-update", "reg-cap-update")
		rec = a.call(t, http.MethodPatch, projectPath(project.Id), adminToken,
			projectDocument(t, map[string]any{"metadata": metadata}))

		assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeValidation)
		kept := projectFrom(t, a.call(t, http.MethodGet, projectPath(project.Id), adminToken, nil))
		if len(kept.Metadata) != 0 {
			t.Errorf("metadata = %v, want the empty object the registration wrote", kept.Metadata)
		}
	})

	t.Run("puts every operation behind the role it needs", func(t *testing.T) {
		existing := registerProject(t, a, adminToken, fixturePlatform, "os-reg-roles", "reg-roles")

		t.Run("no token at all", func(t *testing.T) {
			for _, op := range projectOperations(t, existing, "reg-roles-anonymous") {
				t.Run(op.name, func(t *testing.T) {
					assertChallenged(t, a.call(t, op.method, op.target, "", op.body))
				})
			}
		})

		for _, tc := range []struct {
			name string
			// pair is the external id this case's registration names, so that the
			// one case whose registration goes through collides with nothing.
			pair  string
			token string
			// reads says whether the token reaches the two reads, admin whether it
			// reaches the two writes.
			reads bool
			admin bool
		}{
			{name: "a project token", pair: "reg-roles-project", token: projectToken},
			{name: "a read_all token", pair: "reg-roles-read-all", token: readAllToken, reads: true},
			{name: "an admin token", pair: "reg-roles-admin", token: adminToken, reads: true, admin: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				for _, op := range projectOperations(t, existing, tc.pair) {
					t.Run(op.name, func(t *testing.T) {
						allowed := tc.reads
						if op.admin {
							allowed = tc.admin
						}
						want := http.StatusForbidden
						if allowed {
							want = op.served
						}

						rec := a.call(t, op.method, op.target, tc.token, op.body)

						if got := rec.Code; got != want {
							t.Errorf("status = %d, want %d (body %q)", got, want, rec.Body)
						}
						if want == http.StatusForbidden {
							assertProblem(t, rec, http.StatusForbidden, problem.TypeForbidden)
						}
					})
				}
			})
		}

		// The two registrations the matrix refused wrote nothing, which is only
		// readable on pairs nothing else registers.
		for _, pair := range []string{"reg-roles-anonymous", "reg-roles-project", "reg-roles-read-all"} {
			list := projectListOf(t, a.call(t, http.MethodGet,
				projectsRoute+"?external_id="+pair, adminToken, nil))
			if len(list.Items) != 0 {
				t.Errorf("%s is registered, want a refused registration to write nothing", pair)
			}
		}
	})
}

// TestProjectRegistryReportsAFailedQuery drives the failure paths of the four
// registry operations. With authentication disabled nothing reads the database
// before the handler does, so a database that is gone stops the request inside
// the handler rather than at the credential lookup in front of it.
func TestProjectRegistryReportsAFailedQuery(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIInMode(t, db.Store, auth.ModeDisabled)

	// The bodies and the path are built while the database is still there, so
	// nothing but the request itself runs after it is gone.
	registration := projectDocument(t, map[string]any{
		"platform": fixturePlatform, "cloud": "os-reg-outage", "external_id": "reg-outage",
	})
	update := projectDocument(t, map[string]any{"name": "Renamed during an outage"})
	target := projectPath(uuid.New())

	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(t.Context(), &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}

	for _, tc := range []struct {
		name   string
		method string
		target string
		body   []byte
		detail string
	}{
		{
			name: "the list", method: http.MethodGet, target: projectsRoute,
			detail: "the projects could not be read",
		},
		{
			// A project that cannot be read is not a project that is not there, so
			// the answer is a 500 rather than the 404 of an unknown id.
			name: "the read", method: http.MethodGet, target: target,
			detail: "the project could not be read",
		},
		{
			name: "the registration", method: http.MethodPost, target: projectsRoute,
			body: registration, detail: "the project could not be registered",
		},
		{
			name: "the update", method: http.MethodPatch, target: target,
			body: update, detail: "the project could not be updated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := a.call(t, tc.method, tc.target, "", tc.body)

			assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
			if got := problemDetail(t, rec); got != tc.detail {
				t.Errorf("detail = %q, want %q", got, tc.detail)
			}
		})
	}
}

// projectOperation is one of the four registry operations as the role matrix
// exercises it: which status a call that gets through answers, and whether the
// route takes the admin role rather than read_all.
type projectOperation struct {
	name   string
	method string
	target string
	body   []byte
	admin  bool
	served int
}

// projectOperations builds the four operations against one registered project.
// The registration names externalID, so two cases of the matrix never ask for
// the same pair.
func projectOperations(t *testing.T, existing Project, externalID string) []projectOperation {
	t.Helper()

	return []projectOperation{
		{
			name: "the list", method: http.MethodGet, target: projectsRoute,
			served: http.StatusOK,
		},
		{
			name: "the read", method: http.MethodGet, target: projectPath(existing.Id),
			served: http.StatusOK,
		},
		{
			name: "the registration", method: http.MethodPost, target: projectsRoute,
			admin: true, served: http.StatusCreated,
			body: projectDocument(t, map[string]any{
				"platform": fixturePlatform, "cloud": "os-reg-roles", "external_id": externalID,
			}),
		},
		{
			name: "the update", method: http.MethodPatch, target: projectPath(existing.Id),
			admin: true, served: http.StatusOK,
			body: projectDocument(t, map[string]any{"name": "Renamed by " + externalID}),
		},
	}
}

// projectPath is the route one project is read and updated under.
func projectPath(id uuid.UUID) string {
	return projectsRoute + "/" + id.String()
}

// projectDocument renders a request body from the members it carries. A body is
// built member by member rather than from a generated model, because what an
// update leaves out is part of what the tests assert.
func projectDocument(t *testing.T, members map[string]any) []byte {
	t.Helper()

	raw, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshaling the request body: %v", err)
	}
	return raw
}

// registerProject registers one project over POST and returns it. The setup goes
// through the API so that the rows the reads walk are the ones the write path
// wrote.
func registerProject(t *testing.T, a api, token, platform, cloud, externalID string) Project {
	t.Helper()

	return registeredProject(t, a.call(t, http.MethodPost, projectsRoute, token,
		projectDocument(t, map[string]any{
			"platform": platform, "cloud": cloud, "external_id": externalID,
		})))
}

// registeredProject decodes the answer of a registration, which the contract
// promises is a 201 carrying the project.
func registeredProject(t *testing.T, rec *httptest.ResponseRecorder) Project {
	t.Helper()

	if got := rec.Code; got != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusCreated, rec.Body)
	}
	return decodeProject(t, rec)
}

// projectFrom decodes the answer of a read or an update, which the contract
// promises is a 200 carrying one project.
func projectFrom(t *testing.T, rec *httptest.ResponseRecorder) Project {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	return decodeProject(t, rec)
}

// decodeProject reads one project out of an answer whose status a caller has
// already checked.
func decodeProject(t *testing.T, rec *httptest.ResponseRecorder) Project {
	t.Helper()

	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var got Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	return got
}

// projectListOf decodes the answer of a list call, which the contract promises
// is a 200 carrying one page.
func projectListOf(t *testing.T, rec *httptest.ResponseRecorder) ProjectList {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}

	var list ProjectList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	return list
}

// projectKeys names the projects of a page by the pair the registry is keyed
// by, in the order they were served.
func projectKeys(items []Project) []string {
	keys := make([]string, len(items))
	for i, item := range items {
		keys[i] = item.Cloud + ":" + item.ExternalId
	}
	return keys
}

// memberOnTheWire is one member of an answer as the answer spells it, before a
// generated model has parsed it into a Go value.
func memberOnTheWire(t *testing.T, rec *httptest.ResponseRecorder, name string) string {
	t.Helper()

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	raw, ok := body[name]
	if !ok {
		t.Fatalf("the body %q carries no %s", rec.Body.String(), name)
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decoding %s of %q: %v", name, rec.Body.String(), err)
	}
	return value
}

// fieldErrorLocations names where a rejected request went wrong, which is what
// separates a refusal of the path parameter from one of anything else.
func fieldErrorLocations(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()

	var body struct {
		Errors []problem.FieldError `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}

	locations := make([]string, len(body.Errors))
	for i, entry := range body.Errors {
		locations[i] = entry.Loc
	}
	return locations
}

// projectAudits returns every audit_log row actor left behind under action for
// one project, oldest first.
func projectAudits(t *testing.T, a api, actor, action string, id uuid.UUID) []auditRow {
	t.Helper()

	var found []auditRow
	for _, row := range auditRows(t, a, actor, action) {
		if row.objectID == id.String() {
			found = append(found, row)
		}
	}
	return found
}
