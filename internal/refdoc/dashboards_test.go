package refdoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realDashboards is the directory Grafana is provisioned from, and the number
// of files a page states.
const (
	realDashboards      = "../../deploy/kubernetes/base/grafana/dashboards"
	realDashboardsCount = 4
)

// readDashboards returns every provisioned dashboard, keyed by file name, which
// is what the renderer takes.
func readDashboards(t *testing.T, dir string) map[string][]byte {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		files[entry.Name()] = readRepositoryFile(t, filepath.Join(dir, entry.Name()))
	}
	return files
}

func TestDashboards(t *testing.T) {
	got, err := Dashboards(map[string][]byte{"dashboard.json": readFixture(t, "dashboard.json")})
	if err != nil {
		t.Fatalf("Dashboards() error = %v, want nil", err)
	}

	assertWant(t, "dashboard.want.md", got)
}

func TestDashboardsRendersEachPanelShape(t *testing.T) {
	got, err := Dashboards(map[string][]byte{"dashboard.json": readFixture(t, "dashboard.json")})
	if err != nil {
		t.Fatalf("Dashboards() error = %v, want nil", err)
	}

	for _, want := range []string{
		"### `dashboard.json`",
		"Title `Tally / Fixture`, uid `tally-fixture`.",
		// A variable a viewer types a value into carries its default as a
		// query, and one with nothing to query carries none.
		"| `api_base` | no | `https://api.tally.127-0-0-1.nip.io:8443` |",
		"| `interval` | no | none |",
		// A row carries no query of its own, and the panel it nests is
		// rendered as a panel like any other.
		"| Ingest | `row` | none |",
		"| Event ingest rate | `timeseries` | `sum by (cloud) " +
			"(rate(tally_events_ingested_total{cloud=~\"$cloud\"}[5m]))` |",
		// A panel drawing no query at all.
		"| How to read this | `text` | none |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
	// A panel with two queries is two rows.
	if n := strings.Count(got, "| Event ingest rate | `timeseries` | "); n != 2 {
		t.Errorf("the panel with two queries rendered %d rows, want 2:\n%s", n, got)
	}
}

func TestDashboardsRendersTheDashboardsOfThisRepository(t *testing.T) {
	got, err := Dashboards(readDashboards(t, realDashboards))
	if err != nil {
		t.Fatalf("Dashboards() error = %v, want nil", err)
	}

	if n := countHeadings(got, "### "); n != realDashboardsCount {
		t.Errorf("the directory rendered %d dashboards, want %d", n, realDashboardsCount)
	}
	// The panel drawing one query per resource type, and the one that draws
	// none because it explains the others.
	if n := strings.Count(got, "| Resources by type | "); n != 6 {
		t.Errorf("Resources by type rendered %d queries, want 6", n)
	}
	if want := "| Drift interpretation | `text` | none |"; !strings.Contains(got, want) {
		t.Errorf("the rendering does not carry %q", want)
	}
}

func TestDashboardsRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		files map[string][]byte
		want  string
	}{
		"no dashboards": {
			files: nil,
			want:  "refdoc: no dashboards",
		},
		"a file that is not a model": {
			files: map[string][]byte{"broken.json": []byte("{\n")},
			want:  "refdoc: broken.json: unexpected end of JSON input",
		},
		"a query the site would read as its own": {
			files: map[string][]byte{"mustache.json": []byte(
				`{"panels": [{"title": "Rate", "type": "timeseries",` +
					` "targets": [{"expr": "sum by (cloud) (x) {{ $value }}"}]}]}`)},
			want: `refdoc: mustache.json: "sum by (cloud) (x) {{ $value }}" holds a ` +
				`backtick, a mustache or a line break, render it fenced`,
		},
		// PromQL quotes a string with backticks where a regex would otherwise
		// have to double its backslashes, and a cell is not fenced either, so
		// the span the expression stands in would close on the first of them.
		"a query quoting a string with backticks": {
			files: map[string][]byte{"backtick.json": []byte(
				`{"panels": [{"title": "Rate", "type": "timeseries",` +
					` "targets": [{"expr": "sum(x{cloud=~` + "`" + `os-.*` + "`" + `})"}]}]}`)},
			want: "refdoc: backtick.json: \"sum(x{cloud=~`os-.*`})\" holds a backtick, " +
				"a mustache or a line break, render it fenced",
		},
		// The five strings beside the expression stand in a cell or in the
		// sentence above the tables, and a panel title is not even a code span:
		// it reaches the site as the prose it is written in.
		"a panel title the site would read as its own": {
			files: map[string][]byte{"title.json": []byte(
				`{"panels": [{"title": "Rate {{ $value }}", "type": "timeseries"}]}`)},
			want: `refdoc: title.json: "Rate {{ $value }}" holds a backtick, a mustache ` +
				`or a line break, render it fenced`,
		},
		"a panel type quoted with backticks": {
			files: map[string][]byte{"type.json": []byte(
				`{"panels": [{"title": "Rate", "type": "time` + "`" + `series"}]}`)},
			want: "refdoc: type.json: \"time`series\" holds a backtick, a mustache " +
				"or a line break, render it fenced",
		},
		// Grafana stores the query of a panel the way it is written, and a row
		// of a table ends at the first line break: the rest of the query and
		// every row below it would be read as body text outside the table.
		"a query written over several lines": {
			files: map[string][]byte{"lines.json": []byte(
				`{"panels": [{"title": "Rate", "type": "timeseries",` +
					` "targets": [{"expr": "sum by (cloud) (\n  rate(x[5m])\n)"}]}]}`)},
			want: "refdoc: lines.json: \"sum by (cloud) (\\n  rate(x[5m])\\n)\" holds a " +
				"backtick, a mustache or a line break, render it fenced",
		},
		"a variable query the site would read as its own": {
			files: map[string][]byte{"variable.json": []byte(
				`{"templating": {"list": [{"name": "cloud", "query": "label_values({{ x }})"}]}}`)},
			want: `refdoc: variable.json: "label_values({{ x }})" holds a backtick, ` +
				`a mustache or a line break, render it fenced`,
		},
		"a dashboard title quoted with backticks": {
			files: map[string][]byte{"model.json": []byte(`{"title": "Tally ` + "`" + `beta` + "`" + `"}`)},
			want: "refdoc: model.json: \"Tally `beta`\" holds a backtick, a mustache " +
				"or a line break, render it fenced",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Dashboards(tc.files)
			if err == nil {
				t.Fatalf("Dashboards() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("Dashboards() error = %q, want %q", err, tc.want)
			}
		})
	}
}
