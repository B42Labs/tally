// Package grafana_test pins the JSON contract of the provisioned dashboards:
// the file set the ConfigMap ships, that every file parses, the fixed uids and
// titles, the datasource each query target names, the template variables the
// expressions read, and the drift note. Grafana loads these files at startup
// and reports a broken one only in its own log, so without this test a
// truncated file or a renamed datasource reaches a cluster before anyone sees
// it. The test reads the files from disk and needs no Grafana and no cluster.
package grafana_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// dashboardDir holds the files the provisioning ConfigMap generates from.
const dashboardDir = "dashboards"

// datasourceUID is the uid of the provisioned VictoriaMetrics datasource. A
// target naming anything else queries a datasource Grafana does not have and
// renders an error where the panel should be.
const datasourceUID = "victoriametrics"

// sharedVariables are the variables every dashboard filters by. Both are
// multi-select, which is what lets the "All" option expand to the .* the
// expressions match against.
var sharedVariables = []string{"platform", "cloud"}

// dashboards is the full set of provisioned files, each with the uid saved
// links and dashboard links address it by, and the variables it carries beyond
// the shared two.
var dashboards = map[string]struct {
	uid       string
	variables []string
}{
	"fleet-overview.json":       {uid: "tally-fleet-overview"},
	"project-drilldown.json":    {uid: "tally-project-drilldown", variables: []string{"project_id", "api_base"}},
	"ingestion-health.json":     {uid: "tally-ingestion-health"},
	"reconciliation-drift.json": {uid: "tally-reconciliation-drift"},
}

// dashboard is the part of the Grafana model this test asserts over. Every
// other field passes through untouched.
type dashboard struct {
	UID        string  `json:"uid"`
	Title      string  `json:"title"`
	Panels     []panel `json:"panels"`
	Templating struct {
		List []templateVar `json:"list"`
	} `json:"templating"`
}

type templateVar struct {
	Name  string `json:"name"`
	Multi bool   `json:"multi"`
	Query string `json:"query"`
}

// panel carries a nested panel list of its own: a row panel holds the panels
// below it once the row is collapsed.
type panel struct {
	Title   string   `json:"title"`
	Panels  []panel  `json:"panels"`
	Targets []target `json:"targets"`
}

type target struct {
	RefID      string `json:"refId"`
	Datasource struct {
		UID string `json:"uid"`
	} `json:"datasource"`
}

func TestDashboardDirectory(t *testing.T) {
	t.Run("holds exactly the provisioned files", func(t *testing.T) {
		// The ConfigMap generator ships the whole directory, so a file that is
		// added without a test is provisioned without one too, and a file that is
		// renamed leaves the dashboard it stood for missing from Grafana.
		entries, err := os.ReadDir(dashboardDir)
		if err != nil {
			t.Fatalf("reading %s: %v", dashboardDir, err)
		}

		present := make(map[string]bool, len(entries))
		for _, entry := range entries {
			present[entry.Name()] = true
		}

		var missing, unexpected []string
		for name := range dashboards {
			if !present[name] {
				missing = append(missing, name)
			}
		}
		for name := range present {
			if _, known := dashboards[name]; !known {
				unexpected = append(unexpected, name)
			}
		}
		slices.Sort(missing)
		slices.Sort(unexpected)

		if len(missing) > 0 {
			t.Errorf("%s is missing %v", dashboardDir, missing)
		}
		if len(unexpected) > 0 {
			t.Errorf("%s holds %v, which no test pins", dashboardDir, unexpected)
		}
	})
}

func TestDashboards(t *testing.T) {
	for name, want := range dashboards {
		t.Run(name, func(t *testing.T) {
			// A file that does not parse fails here, with the position
			// encoding/json reports. Grafana would skip it just as silently.
			d := load(t, name)

			t.Run("carries its fixed uid and a title", func(t *testing.T) {
				if d.UID != want.uid {
					t.Errorf("uid = %q, want %q; a changed uid breaks every saved link", d.UID, want.uid)
				}
				if d.Title == "" {
					t.Error("title is empty, so the dashboard list shows the file under no name")
				}
			})

			t.Run("queries VictoriaMetrics from every target", func(t *testing.T) {
				var targets int
				for _, p := range flatten(d.Panels) {
					targets += len(p.Targets)
					for _, tgt := range p.Targets {
						if tgt.Datasource.UID != datasourceUID {
							t.Errorf("panel %q target %q names datasource uid %q, want %q",
								p.Title, tgt.RefID, tgt.Datasource.UID, datasourceUID)
						}
					}
				}
				if targets == 0 {
					t.Error("no panel carries a query target, so the dashboard renders nothing")
				}
			})

			t.Run("declares the variables its expressions read", func(t *testing.T) {
				declared := make(map[string]templateVar, len(d.Templating.List))
				for _, v := range d.Templating.List {
					declared[v.Name] = v
				}

				for _, variable := range sharedVariables {
					v, ok := declared[variable]
					if !ok {
						t.Errorf("no %q variable, so the panels filter by an unresolved $%s", variable, variable)
						continue
					}
					if !v.Multi {
						t.Errorf("variable %q is not multi-select, so it cannot expand to more than one value", variable)
					}
				}
				for _, variable := range want.variables {
					if _, ok := declared[variable]; !ok {
						t.Errorf("no %q variable, so the panels filter by an unresolved $%s", variable, variable)
					}
				}
			})
		})
	}
}

func TestProjectDrilldown(t *testing.T) {
	t.Run("reads the project list from a per-project series", func(t *testing.T) {
		// label_values scans every series its matcher selects, and refresh 1
		// re-runs it on every dashboard load. The per-resource series are the
		// wrong thing to scan: openstack_nova_server_status carries one series
		// per instance and openstack_cinder_volume_status one per volume, and
		// four of the nova labels change on reboot, migration and address
		// reassignment, so the set over the dashboard window is a multiple of
		// the fleet rather than equal to it. The limits gauge carries one
		// series per project, which is the list the dropdown wants.
		const want = "label_values(openstack_nova_limits_instances_used, tenant_id)"

		d := load(t, "project-drilldown.json")

		for _, v := range d.Templating.List {
			if v.Name != "project_id" {
				continue
			}
			if v.Query != want {
				t.Errorf("project_id query = %q, want %q", v.Query, want)
			}
			return
		}
		t.Error("project-drilldown.json declares no project_id variable")
	})
}

func TestReconciliationDrift(t *testing.T) {
	t.Run("keeps the note that reads the drift numbers", func(t *testing.T) {
		// The panel is what tells an operator that a created or deleted count is a
		// missed event rather than routine reconciliation work.
		const note = "investigate before period finalization"

		raw := read(t, "reconciliation-drift.json")

		if !bytes.Contains(raw, []byte(note)) {
			t.Errorf("reconciliation-drift.json carries no %q", note)
		}
	})
}

// load reads and decodes one dashboard file.
func load(t *testing.T, name string) dashboard {
	t.Helper()

	var d dashboard
	if err := json.Unmarshal(read(t, name), &d); err != nil {
		t.Fatalf("parsing %s: %v", filepath.Join(dashboardDir, name), err)
	}
	return d
}

// read returns the raw bytes of one dashboard file.
func read(t *testing.T, name string) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dashboardDir, name))
	if err != nil {
		t.Fatalf("reading the dashboard: %v", err)
	}
	return raw
}

// flatten returns every panel of the dashboard, including the ones a row panel
// nests under itself.
func flatten(panels []panel) []panel {
	all := make([]panel, 0, len(panels))
	for _, p := range panels {
		all = append(all, p)
		all = append(all, flatten(p.Panels)...)
	}
	return all
}
