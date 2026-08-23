package counters_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/source"
)

// roadmapSources is the file the roadmap gives as the example of both kinds:
// one metricsql source with every placeholder a query may use, and one events
// source.
const roadmapSources = `sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: >
      sum(increase(ceilometer_network_outgoing_bytes{cloud="{cloud}",
          resource_id="{resource_id}"}[{window}])) / 1e9
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull
`

// writeSources writes a counter sources file and returns its path.
func writeSources(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "counter-sources.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	t.Run("an empty path configures no counter source", func(t *testing.T) {
		cfg, err := counters.Load("")
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if len(cfg.Sources) != 0 {
			t.Errorf("Sources = %#v, want none", cfg.Sources)
		}
	})

	t.Run("a path that does not exist is an error, not a run without counters", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.yaml")

		_, err := counters.Load(path)
		if err == nil {
			t.Fatal("Load() error = nil, want an error")
		}
		if want := "reading the counter sources " + path + ":"; !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %q, want it to contain %q", err, want)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Load() error = %v, want it to wrap fs.ErrNotExist", err)
		}
	})

	t.Run("the roadmap's file yields both kinds in file order", func(t *testing.T) {
		cfg, err := counters.Load(writeSources(t, roadmapSources))
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if len(cfg.Sources) != 2 {
			t.Fatalf("Sources = %d entries, want 2", len(cfg.Sources))
		}

		egress := cfg.Sources[0]
		if egress.Platform != "openstack" || egress.ResourceType != "instance" || egress.Metric != "egress_gb" {
			t.Errorf("Sources[0] measures %s of %s/%s, want egress_gb of openstack/instance",
				egress.Metric, egress.Platform, egress.ResourceType)
		}
		if egress.Kind != counters.KindMetricsQL {
			t.Errorf("Sources[0].Kind = %q, want %q", egress.Kind, counters.KindMetricsQL)
		}
		for _, placeholder := range []string{"{cloud}", "{resource_id}", "{window}"} {
			if !strings.Contains(egress.Query, placeholder) {
				t.Errorf("Sources[0].Query = %q, want it to contain %q", egress.Query, placeholder)
			}
		}
		if egress.EventType != "" {
			t.Errorf("Sources[0].EventType = %q, want it empty", egress.EventType)
		}
		// The file says nothing about required, so the source is required.
		if !egress.Required {
			t.Error("Sources[0].Required = false, want true")
		}

		pulls := cfg.Sources[1]
		if pulls.Platform != "harbor" || pulls.ResourceType != "repository" || pulls.Metric != "pulls" {
			t.Errorf("Sources[1] measures %s of %s/%s, want pulls of harbor/repository",
				pulls.Metric, pulls.Platform, pulls.ResourceType)
		}
		if pulls.Kind != counters.KindEvents {
			t.Errorf("Sources[1].Kind = %q, want %q", pulls.Kind, counters.KindEvents)
		}
		if pulls.EventType != "repository.pull" {
			t.Errorf("Sources[1].EventType = %q, want %q", pulls.EventType, "repository.pull")
		}
		if pulls.Query != "" {
			t.Errorf("Sources[1].Query = %q, want it empty", pulls.Query)
		}

		if !cfg.HasMetricsQL() {
			t.Error("HasMetricsQL() = false, want true")
		}
	})

	t.Run("a file that does not parse is reported by its path", func(t *testing.T) {
		path := writeSources(t, "sources: [\n")

		_, err := counters.Load(path)
		if err == nil {
			t.Fatal("Load() error = nil, want an error")
		}
		if prefix := path + ": "; !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("Load() error = %q, want it to start with %q", err, prefix)
		}
		if want := "parsing the counter sources:"; !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %q, want it to contain %q", err, want)
		}
	})
}

func TestParseAcceptsDocumentsWithoutSources(t *testing.T) {
	documents := map[string]string{
		"an empty document":        "",
		"only a comment":           "# only a comment\n",
		"an empty mapping":         "{}\n",
		"a bare document marker":   "---\n",
		"an empty list of sources": "sources: []\n",
	}

	for name, document := range documents {
		t.Run(name, func(t *testing.T) {
			cfg, err := counters.Parse([]byte(document))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if len(cfg.Sources) != 0 {
				t.Errorf("Sources = %#v, want none", cfg.Sources)
			}
		})
	}

	t.Run("no content at all", func(t *testing.T) {
		cfg, err := counters.Parse(nil)
		if err != nil {
			t.Fatalf("Parse() error = %v, want nil", err)
		}
		if len(cfg.Sources) != 0 {
			t.Errorf("Sources = %#v, want none", cfg.Sources)
		}
	})
}

func TestParseRejectsAMalformedDocument(t *testing.T) {
	t.Run("content that is not YAML", func(t *testing.T) {
		_, err := counters.Parse([]byte("sources: [\n"))
		if err == nil {
			t.Fatal("Parse() error = nil, want an error")
		}
		if want := "parsing the counter sources:"; !strings.Contains(err.Error(), want) {
			t.Errorf("Parse() error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("a misspelled key is named rather than ignored", func(t *testing.T) {
		_, err := counters.Parse([]byte(`sources:
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_typ: repository.pull
`))
		if err == nil {
			t.Fatal("Parse() error = nil, want an error")
		}
		if want := "parsing the counter sources:"; !strings.Contains(err.Error(), want) {
			t.Errorf("Parse() error = %q, want it to contain %q", err, want)
		}
		if want := "event_typ"; !strings.Contains(err.Error(), want) {
			t.Errorf("Parse() error = %q, want it to name %q", err, want)
		}
	})

	// Only the first document is decoded. A second one, appended to add
	// another team's counters, would otherwise leave its sources out of every
	// draft of the period without anything saying so.
	t.Run("a second document is refused rather than dropped", func(t *testing.T) {
		_, err := counters.Parse([]byte(`sources:
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull
---
sources:
  - platform: harbor
    resource_type: repository
    metric: pushes
    kind: events
    event_type: repository.push
`))
		if err == nil {
			t.Fatal("Parse() error = nil, want an error")
		}
		want := "the file holds more than one document, every source belongs in the first"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse() error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("a second document that is not YAML is named too", func(t *testing.T) {
		_, err := counters.Parse([]byte("sources: []\n---\nsources: [\n"))
		if err == nil {
			t.Fatal("Parse() error = nil, want an error")
		}
		if want := "parsing the counter sources:"; !strings.Contains(err.Error(), want) {
			t.Errorf("Parse() error = %q, want it to contain %q", err, want)
		}
	})
}

func TestParseRejectsInvalidSources(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "an entry without a platform",
			content: `sources:
  - resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull
`,
			wantErr: "sources[0]: platform must be set",
		},
		{
			name: "an entry without a resource type",
			content: `sources:
  - platform: harbor
    metric: pulls
    kind: events
    event_type: repository.pull
`,
			wantErr: "sources[0]: resource_type must be set",
		},
		{
			name: "an entry without a metric",
			content: `sources:
  - platform: harbor
    resource_type: repository
    kind: events
    event_type: repository.pull
`,
			wantErr: "sources[0]: metric must be set",
		},
		{
			name: "a counter claiming the minutes the engine derives",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: minutes
    kind: events
    event_type: repository.pull
`,
			wantErr: `sources[0]: metric "minutes" is reserved by the engine`,
		},
		{
			name: "a counter claiming the count the engine derives",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: count
    kind: events
    event_type: repository.pull
`,
			wantErr: `sources[0]: metric "count" is reserved by the engine`,
		},
		{
			name: "a kind the engine cannot measure",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: gauge
    event_type: repository.pull
`,
			wantErr: `sources[0]: kind "gauge" must be events or metricsql`,
		},
		{
			name: "an entry without a kind",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: pulls
    event_type: repository.pull
`,
			wantErr: `sources[0]: kind "" must be events or metricsql`,
		},
		{
			name: "an events source without an event type",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
`,
			wantErr: "sources[0]: event_type must be set",
		},
		{
			name: "an events source carrying a query",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull
    query: 'sum(pulls)'
`,
			wantErr: "sources[0]: query applies to metricsql sources only",
		},
		{
			name: "an events source declared required",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull
    required: true
`,
			wantErr: "sources[0]: required applies to metricsql sources only",
		},
		{
			name: "an events source opting out of being required",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull
    required: false
`,
			wantErr: "sources[0]: required applies to metricsql sources only",
		},
		{
			name: "a metricsql source without a query",
			content: `sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
`,
			wantErr: "sources[0]: query must be set",
		},
		{
			name: "a metricsql source carrying an event type",
			content: `sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: 'sum(egress)'
    event_type: instance.egress
`,
			wantErr: "sources[0]: event_type applies to events sources only",
		},
		{
			name: "a query using a placeholder the engine does not render",
			content: `sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: 'sum(egress{tenant="{tenant}"})'
`,
			wantErr: "sources[0]: query uses the unknown placeholder {tenant}",
		},
		{
			name: "a query matching a resource id with a regex matcher",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: egress_gb
    kind: metricsql
    query: 'sum(metric{repository=~"{resource_id}"})'
`,
			wantErr: "sources[0]: query matches {resource_id} with =~ or !~, which reads the substituted " +
				"value as a pattern; an identity is matched with = or !=",
		},
		{
			name: "a query matching a cloud with a negated regex matcher",
			content: `sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: 'sum(metric{cloud!~"prefix-{cloud}"})'
`,
			wantErr: "sources[0]: query matches {cloud} with =~ or !~, which reads the substituted " +
				"value as a pattern; an identity is matched with = or !=",
		},
		{
			// A quantifier is written with the braces and the comma the guard
			// once stopped at, so the placeholder behind it stayed unseen.
			name: "a resource id behind a quantifier in a regex matcher",
			content: `sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: 'sum(rate(cpu{instance=~"node-[0-9]{1,3}-{resource_id}"}[{window}]))'
`,
			wantErr: "sources[0]: query matches {resource_id} with =~ or !~, which reads the substituted " +
				"value as a pattern; an identity is matched with = or !=",
		},
		{
			name: "a resource id behind a quantifier in a negated regex matcher",
			content: `sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: 'sum(rate(cpu{pod!~"sys-.{0,8}-{resource_id}"}[{window}]))'
`,
			wantErr: "sources[0]: query matches {resource_id} with =~ or !~, which reads the substituted " +
				"value as a pattern; an identity is matched with = or !=",
		},
		{
			// The same hazard with no =~ anywhere: the pattern is an argument.
			name: "an identity in the pattern argument of label_replace",
			content: `sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: 'sum(label_replace(cpu, "d", "$1", "instance", "{resource_id}.*"))'
`,
			wantErr: "sources[0]: query calls label_replace, whose pattern argument would read a " +
				"substituted identity as a pattern",
		},
		{
			name: "an identity in the pattern argument of label_match",
			content: `sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: 'sum(label_match(cpu, "instance", "{cloud}"))'
`,
			wantErr: "sources[0]: query calls label_match, whose pattern argument would read a " +
				"substituted identity as a pattern",
		},
		{
			name: "one metric of one resource type configured twice",
			content: `sources:
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull.v2
`,
			wantErr: "sources[1]: pulls of harbor/repository is configured twice",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := counters.Parse([]byte(tc.content))
			if err == nil {
				t.Fatalf("Parse() error = nil, want %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("Parse() error = %q, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseAcceptsQueriesWithLabelSelectors(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "selectors holding every placeholder a query may use",
			query: `metric{cloud="{cloud}",resource_id="{resource_id}"}[{window}]`,
		},
		{
			name:  "the project placeholder",
			query: `sum(metric{project="{project_id}"})`,
		},
		{
			name:  "an empty selector, which holds no placeholder",
			query: `metric{}`,
		},
		{
			// The regex matcher reads a value the file wrote itself, and the
			// identity beside it is matched as the literal it is inert as.
			name:  "a regex matcher over a label no identity is substituted into",
			query: `sum(metric{job=~"vm.*",resource_id="{resource_id}"})`,
		},
		{
			// The braces of a quantifier belong to the matcher's own pattern,
			// not to a placeholder, and neither ends the literal the guard
			// reads.
			name:  "a quantifier in a regex matcher over a label no identity reaches",
			query: `sum(rate(cpu{instance=~"node-[0-9]{1,3}",resource_id="{resource_id}"}[{window}]))`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := counters.Parse([]byte(`sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: '` + tc.query + `'
`))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if len(cfg.Sources) != 1 {
				t.Fatalf("Sources = %d entries, want 1", len(cfg.Sources))
			}
			if cfg.Sources[0].Query != tc.query {
				t.Errorf("Sources[0].Query = %q, want %q", cfg.Sources[0].Query, tc.query)
			}
		})
	}
}

func TestParseResolvesRequired(t *testing.T) {
	tests := []struct {
		name     string
		required string
		want     bool
	}{
		{name: "a metricsql source is required unless the file says otherwise", required: "", want: true},
		{name: "a metricsql source can opt out of being required", required: "    required: false\n", want: false},
		{name: "a metricsql source can say so explicitly", required: "    required: true\n", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := counters.Parse([]byte(`sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: 'sum(egress)'
` + tc.required))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if len(cfg.Sources) != 1 {
				t.Fatalf("Sources = %d entries, want 1", len(cfg.Sources))
			}
			if got := cfg.Sources[0].Required; got != tc.want {
				t.Errorf("Sources[0].Required = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestHasMetricsQL(t *testing.T) {
	events := counters.Source{
		Platform: "harbor", ResourceType: "repository", Metric: "pulls",
		Kind: counters.KindEvents, EventType: "repository.pull", Required: true,
	}
	metricsQL := counters.Source{
		Platform: "openstack", ResourceType: "instance", Metric: "egress_gb",
		Kind: counters.KindMetricsQL, Query: "sum(egress)", Required: true,
	}

	tests := []struct {
		name string
		cfg  counters.Config
		want bool
	}{
		{
			name: "one metricsql source among events sources needs a client",
			cfg:  counters.Config{Sources: []counters.Source{events, metricsQL, events}},
			want: true,
		},
		{
			name: "events sources alone need no client",
			cfg:  counters.Config{Sources: []counters.Source{events}},
			want: false,
		},
		{
			name: "no source at all needs no client",
			cfg:  counters.Config{},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.HasMetricsQL(); got != tc.want {
				t.Errorf("HasMetricsQL() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestWindow(t *testing.T) {
	tests := []struct {
		name    string
		seconds int64
		want    string
	}{
		{name: "a month of 30 days is whole hours", seconds: 1296000, want: "360h"},
		{name: "an hour and a half is whole minutes", seconds: 5400, want: "90m"},
		{name: "a minute and a second is neither", seconds: 61, want: "61s"},
		{name: "one hour", seconds: 3600, want: "1h"},
		{name: "one minute", seconds: 60, want: "1m"},
		// A draft shorter than a second still needs a window MetricsQL
		// measures over, and [0s] is not one.
		{name: "a draft shorter than a second", seconds: 0, want: "1s"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := counters.Window(tc.seconds); got != tc.want {
				t.Errorf("Window(%d) = %q, want %q", tc.seconds, got, tc.want)
			}
		})
	}
}

func TestRenderQuery(t *testing.T) {
	t.Run("a query is rendered for the draft it measures", func(t *testing.T) {
		tests := []struct {
			name                         string
			query                        string
			cloud, resourceID, projectID string
			want                         string
		}{
			{
				name:  "the roadmap's query",
				query: `sum(increase(ceilometer_network_outgoing_bytes{cloud="{cloud}", resource_id="{resource_id}"}[{window}])) / 1e9`,
				want:  `sum(increase(ceilometer_network_outgoing_bytes{cloud="os-prod-eu1", resource_id="abc-123"}[360h])) / 1e9`,
			},
			{
				name:  "a query selecting the project",
				query: `sum(metric{cloud="{cloud}", project="{project_id}"})`,
				want:  `sum(metric{cloud="os-prod-eu1", project="proj-456"})`,
			},
			{
				name:       "a repository id, which carries a slash",
				query:      `sum(metric{repository="{resource_id}"})`,
				resourceID: "team-alpha/app",
				want:       `sum(metric{repository="team-alpha/app"})`,
			},
			{
				name:  "an empty query",
				query: "",
				want:  "",
			},
			{
				name:  "a query without placeholders",
				query: "sum(metric)",
				want:  "sum(metric)",
			},
			{
				// The placeholder the value would be refused for is not in
				// this query, so the value never reaches it.
				name:       "a value a query does not substitute",
				query:      `sum(metric{cloud="{cloud}"})`,
				resourceID: `x"} or vector(1e12) #`,
				want:       `sum(metric{cloud="os-prod-eu1"})`,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				cloud, resourceID, projectID := tc.cloud, tc.resourceID, tc.projectID
				if cloud == "" {
					cloud = "os-prod-eu1"
				}
				if resourceID == "" {
					resourceID = "abc-123"
				}
				if projectID == "" {
					projectID = "proj-456"
				}

				got, err := counters.RenderQuery(tc.query, cloud, resourceID, projectID, 1296000)
				if err != nil {
					t.Fatalf("RenderQuery() error = %v, want nil", err)
				}
				if got != tc.want {
					t.Errorf("RenderQuery() = %q, want %q", got, tc.want)
				}
			})
		}
	})

	// The identity values reach the engine from ingested event data, and the
	// query around them is text an operator wrote. Nothing here can tell which
	// of MetricsQL's three string literal forms a placeholder sits in, or
	// whether it sits in one at all, so a value that is not inert is refused
	// rather than escaped for one of them.
	t.Run("an identity a query may not carry is refused", func(t *testing.T) {
		tests := []struct {
			name    string
			query   string
			value   string
			wantErr string
		}{
			{
				name:    "a resource id ending the double-quoted matcher it sits in",
				query:   `sum(increase(metric{resource_id="{resource_id}"}[{window}]))`,
				value:   `x"}[1s])) or vector(1e12) #`,
				wantErr: `the resource_id "x\"}[1s])) or vector(1e12) #" holds a character`,
			},
			{
				name:    "a resource id ending a single-quoted matcher",
				query:   `sum(metric{resource_id='{resource_id}'})`,
				value:   `x'} or vector(1e12) or metric{a='`,
				wantErr: "the resource_id",
			},
			{
				name:    "a resource id ending a backquoted matcher",
				query:   "sum(metric{resource_id=`{resource_id}`})",
				value:   "x`} or vector(1e12) or metric{a=`",
				wantErr: "the resource_id",
			},
			{
				name:    "a resource id holding a backslash",
				query:   `sum(metric{resource_id="{resource_id}"})`,
				value:   `a\b`,
				wantErr: "the resource_id",
			},
			{
				name:    "a resource id holding a line break, which no literal carries",
				query:   `sum(metric{resource_id="{resource_id}"})`,
				value:   "a\nb",
				wantErr: "the resource_id",
			},
			{
				// A regex matcher needs no quote at all to be subverted.
				name:    "a resource id that is a regex matching everything",
				query:   `sum(metric{resource_id=~"{resource_id}"})`,
				value:   ".*",
				wantErr: "the resource_id",
			},
			{
				name:    "a placeholder written outside a literal",
				query:   "sum(metric{resource_id={resource_id}})",
				value:   "x} or vector(1e12) or metric{a=b",
				wantErr: "the resource_id",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := counters.RenderQuery(tc.query, "os-prod-eu1", tc.value, "proj-456", 1296000)
				if err == nil {
					t.Fatalf("RenderQuery() error = nil, want %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("RenderQuery() error = %q, want it to contain %q", err, tc.wantErr)
				}
			})
		}
	})

	// The cloud and the project id come from ingested event data the same way
	// the resource id does, and they land in the same matcher.
	t.Run("every substituted identity is checked, not only the resource id", func(t *testing.T) {
		tests := []struct {
			name                         string
			query                        string
			cloud, resourceID, projectID string
			wantErr                      string
		}{
			{
				name:    "a cloud holding a quote",
				query:   `sum(metric{cloud="{cloud}"})`,
				cloud:   `a"b`,
				wantErr: `the cloud "a\"b" holds a character`,
			},
			{
				name:      "a project id holding a quote",
				query:     `sum(metric{project="{project_id}"})`,
				projectID: `c"d`,
				wantErr:   `the project_id "c\"d" holds a character`,
			},
			{
				name:       "a resource id holding a quote",
				query:      `sum(metric{resource_id="{resource_id}"})`,
				resourceID: `e"f`,
				wantErr:    `the resource_id "e\"f" holds a character`,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				cloud, resourceID, projectID := tc.cloud, tc.resourceID, tc.projectID
				if cloud == "" {
					cloud = "os-prod-eu1"
				}
				if resourceID == "" {
					resourceID = "abc-123"
				}
				if projectID == "" {
					projectID = "proj-456"
				}

				_, err := counters.RenderQuery(tc.query, cloud, resourceID, projectID, 1296000)
				if err == nil {
					t.Fatalf("RenderQuery() error = nil, want %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("RenderQuery() error = %q, want it to contain %q", err, tc.wantErr)
				}
			})
		}
	})
}

// The billing period the merge cases below are measured over: March 2026.
var (
	periodFrom = utc(time.March, 1)
	periodTo   = utc(time.April, 1)
)

// The two resources the merge cases measure.
var (
	repository = source.Resource{
		Cloud: "harbor-prod", Platform: "harbor",
		ResourceType: "repository", ResourceID: "team-alpha/app",
	}
	instance = source.Resource{
		Cloud: "os-prod-eu1", Platform: "openstack",
		ResourceType: "instance", ResourceID: "abc-123",
	}
)

// The queries the merge cases run: the roadmap's egress query for an instance,
// and the same shape over a repository.
const (
	instanceEgress   = `sum(increase(ceilometer_network_outgoing_bytes{cloud="{cloud}", resource_id="{resource_id}"}[{window}])) / 1e9`
	repositoryEgress = `sum(increase(harbor_egress_bytes{cloud="{cloud}", repository="{resource_id}"}[{window}])) / 1e9`
)

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

func withCloud(cloud string) option {
	return func(s *event.Stored) { s.Cloud = cloud }
}

// ev builds a stored event, the way the metering tests build one. Defaults keep
// each case to the dimension it exercises: one OpenStack instance of one
// project, received when it happened.
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

// meterResource folds a history into the drafts the merge is measured into.
func meterResource(t *testing.T, history []event.Stored, from, to time.Time) []metering.UsageDraft {
	t.Helper()

	drafts, err := metering.MeterResource(history, from, to)
	if err != nil {
		t.Fatalf("MeterResource() error = %v, want nil", err)
	}
	return drafts
}

// draft is one usage draft as metering hands it out, for the cases that need a
// single interval rather than a whole history.
func draft(from, to time.Time, usage map[string]any) metering.UsageDraft {
	return metering.UsageDraft{
		State:     "active",
		ProjectID: "proj-456",
		FromTS:    from,
		ToTS:      to,
		Seconds:   int64(to.Sub(from) / time.Second),
		Usage:     usage,
	}
}

// egressSource is the metricsql source of the instance cases.
func egressSource(required bool) counters.Source {
	return counters.Source{
		Platform: "openstack", ResourceType: "instance", Metric: "egress_gb",
		Kind: counters.KindMetricsQL, Query: instanceEgress, Required: required,
	}
}

// pullsSource is the events source of the repository cases. It is not required,
// a flag the merge ignores on an events source.
func pullsSource() counters.Source {
	return counters.Source{
		Platform: "harbor", ResourceType: "repository", Metric: "pulls",
		Kind: counters.KindEvents, EventType: "repository.pull",
	}
}

// rendered is the expression a repository draft of the given length is queried
// with.
func rendered(t *testing.T, query string, seconds int64) string {
	t.Helper()

	expr, err := counters.RenderQuery(query, "harbor-prod", "team-alpha/app", "harbor-team-alpha", seconds)
	if err != nil {
		t.Fatalf("RenderQuery() error = %v, want nil", err)
	}
	return expr
}

// newMeasurer builds the measurer of one case over the sources it measures.
func newMeasurer(t *testing.T, events counters.EventCounter, vm counters.Querier, sources ...counters.Source) *counters.Measurer {
	t.Helper()

	m, err := counters.New(counters.Config{Sources: sources}, events, vm)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return m
}

// countKey is the table entry the fake counter answers a call from.
type countKey struct {
	eventType string
	from      time.Time
}

// countCall is a call the fake counter saw.
type countCall struct {
	resource  source.Resource
	eventType string
	from, to  time.Time
}

// fakeCounter answers from a table keyed by event type and interval start, and
// records every call in order. An interval the table does not name counts zero,
// and err fails every call.
type fakeCounter struct {
	counts map[countKey]int64
	err    error
	calls  []countCall
}

func (f *fakeCounter) CountEvents(_ context.Context, r source.Resource, eventType string, from, to time.Time) (int64, error) {
	f.calls = append(f.calls, countCall{resource: r, eventType: eventType, from: from, to: to})
	if f.err != nil {
		return 0, f.err
	}
	return f.counts[countKey{eventType: eventType, from: from}], nil
}

// queryCall is a call the fake querier saw.
type queryCall struct {
	expr string
	at   time.Time
}

// fakeQuerier answers from a table of decimal literals keyed by the instant it
// is queried at, and records every call in order. err fails every call, or only
// the call at failAt where that is set. A canceled context fails before either,
// the way the real client's request does.
type fakeQuerier struct {
	values map[time.Time]string
	err    error
	failAt *time.Time
	calls  []queryCall
}

func (f *fakeQuerier) Query(ctx context.Context, expr string, at time.Time) (decimal.Decimal, error) {
	f.calls = append(f.calls, queryCall{expr: expr, at: at})
	if err := ctx.Err(); err != nil {
		return decimal.Zero, err
	}
	if f.err != nil && (f.failAt == nil || f.failAt.Equal(at)) {
		return decimal.Zero, f.err
	}

	value, ok := f.values[at]
	if !ok {
		return decimal.Zero, fmt.Errorf("the fake querier has no value at %s", at.Format(time.RFC3339Nano))
	}
	return decimal.NewFromString(value)
}

// quantityOf is the value a draft carries for a counter metric, rendered at the
// four places it reaches JSONB at. Both kinds are read through it: a count and a
// query result reach a usage object as the same quantity.
func quantityOf(t *testing.T, d metering.UsageDraft, metric string) string {
	t.Helper()

	quantity, ok := d.Usage[metric].(money.Quantity)
	if !ok {
		t.Fatalf("Usage[%s] = %#v, want a money.Quantity", metric, d.Usage[metric])
	}
	return quantity.StringFixed(4)
}

func TestNew(t *testing.T) {
	pushesSource := counters.Source{
		Platform: "harbor", ResourceType: "repository", Metric: "pushes",
		Kind: counters.KindEvents, EventType: "repository.push",
	}

	t.Run("events sources without an event counter", func(t *testing.T) {
		cfg := counters.Config{Sources: []counters.Source{pullsSource(), pushesSource}}

		_, err := counters.New(cfg, nil, &fakeQuerier{})
		if err == nil {
			t.Fatal("New() error = nil, want an error")
		}
		want := "2 events sources are configured but no event counter was given"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New() error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("a metricsql source without a querier", func(t *testing.T) {
		cfg := counters.Config{Sources: []counters.Source{egressSource(true)}}

		_, err := counters.New(cfg, &fakeCounter{}, nil)
		if err == nil {
			t.Fatal("New() error = nil, want an error")
		}
		want := "1 metricsql sources are configured but no VictoriaMetrics querier was given"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New() error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("a hand-built source the file form would be refused for", func(t *testing.T) {
		cfg := counters.Config{Sources: []counters.Source{
			{Platform: "harbor", ResourceType: "repository", Metric: "pulls"},
		}}

		_, err := counters.New(cfg, &fakeCounter{}, &fakeQuerier{})
		if err == nil {
			t.Fatal("New() error = nil, want an error")
		}
		want := `sources[0]: kind "" must be events or metricsql`
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New() error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("no source at all needs neither seam", func(t *testing.T) {
		m, err := counters.New(counters.Config{}, nil, nil)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		if m == nil {
			t.Error("New() = nil, want a measurer")
		}
	})
}

func TestApply(t *testing.T) {
	t.Run("example 5: two counts and a query over each draft", func(t *testing.T) {
		identity := []option{
			withPlatform("harbor"), withCloud("harbor-prod"),
			withResource("repository", "team-alpha/app"), withProject("harbor-team-alpha"),
		}
		drafts := meterResource(t, []event.Stored{
			ev("e1", "repository.create", utc(time.February, 14), append(identity,
				withState("active"), withSize(size(t, `{"storage_gb":10}`)))...),
			ev("e2", "repository.push", utc(time.March, 18), append(identity,
				withState("active"), withSize(size(t, `{"storage_gb":15}`)))...),
		}, periodFrom, periodTo)
		if len(drafts) != 2 {
			t.Fatalf("MeterResource() = %d drafts, want 2", len(drafts))
		}
		metered := []map[string]any{maps.Clone(drafts[0].Usage), maps.Clone(drafts[1].Usage)}

		counter := &fakeCounter{counts: map[countKey]int64{
			{eventType: "repository.pull", from: periodFrom}:          812,
			{eventType: "repository.pull", from: utc(time.March, 18)}: 711,
			{eventType: "repository.push", from: periodFrom}:          47,
			{eventType: "repository.push", from: utc(time.March, 18)}: 23,
		}}
		querier := &fakeQuerier{values: map[time.Time]string{
			utc(time.March, 18): "38.5",
			periodTo:            "31.2",
		}}
		m := newMeasurer(t, counter, querier,
			pullsSource(),
			counters.Source{
				Platform: "harbor", ResourceType: "repository", Metric: "pushes",
				Kind: counters.KindEvents, EventType: "repository.push",
			},
			counters.Source{
				Platform: "harbor", ResourceType: "repository", Metric: "egress_gb",
				Kind: counters.KindMetricsQL, Query: repositoryEgress, Required: true,
			},
		)

		warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: repository, Drafts: drafts}})
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
		if warnings != nil {
			t.Errorf("Apply() warnings = %#v, want none", warnings)
		}

		want := []struct {
			pulls, pushes, egress string
		}{
			{pulls: "812.0000", pushes: "47.0000", egress: "38.5000"},
			{pulls: "711.0000", pushes: "23.0000", egress: "31.2000"},
		}
		for i, d := range drafts {
			if got := quantityOf(t, d, "pulls"); got != want[i].pulls {
				t.Errorf("draft %d Usage[pulls] = %s, want %s", i, got, want[i].pulls)
			}
			if got := quantityOf(t, d, "pushes"); got != want[i].pushes {
				t.Errorf("draft %d Usage[pushes] = %s, want %s", i, got, want[i].pushes)
			}
			if got := quantityOf(t, d, "egress_gb"); got != want[i].egress {
				t.Errorf("draft %d Usage[egress_gb] = %s, want %s", i, got, want[i].egress)
			}
			// What metering derived is left as it was.
			for _, metric := range []string{"storage_gb", "minutes", "count"} {
				if got := d.Usage[metric]; got != metered[i][metric] {
					t.Errorf("draft %d Usage[%s] = %#v, want %#v", i, metric, got, metered[i][metric])
				}
			}
		}

		wantCounts := []countCall{
			{resource: repository, eventType: "repository.pull", from: drafts[0].FromTS, to: drafts[0].ToTS},
			{resource: repository, eventType: "repository.push", from: drafts[0].FromTS, to: drafts[0].ToTS},
			{resource: repository, eventType: "repository.pull", from: drafts[1].FromTS, to: drafts[1].ToTS},
			{resource: repository, eventType: "repository.push", from: drafts[1].FromTS, to: drafts[1].ToTS},
		}
		if !reflect.DeepEqual(counter.calls, wantCounts) {
			t.Errorf("the counter saw %+v, want %+v", counter.calls, wantCounts)
		}

		wantQueries := []queryCall{
			{expr: rendered(t, repositoryEgress, drafts[0].Seconds), at: drafts[0].ToTS},
			{expr: rendered(t, repositoryEgress, drafts[1].Seconds), at: drafts[1].ToTS},
		}
		if !reflect.DeepEqual(querier.calls, wantQueries) {
			t.Errorf("the querier saw %+v, want %+v", querier.calls, wantQueries)
		}

		encoded, err := json.Marshal(drafts[0].Usage)
		if err != nil {
			t.Fatalf("Marshal() error = %v, want nil", err)
		}
		// Every counter renders at the four places a usage quantity carries,
		// whichever kind measured it: count is the engine's own key, not a
		// counter.
		wantJSON := `{"count":1,"egress_gb":38.5000,"minutes":24480.0000,` +
			`"pulls":812.0000,"pushes":47.0000,"storage_gb":10}`
		if string(encoded) != wantJSON {
			t.Errorf("Marshal(Usage) = %s, want %s", encoded, wantJSON)
		}
	})

	t.Run("the end-to-end example: one query per power cycle draft", func(t *testing.T) {
		powered := `{"vcpus":4,"ram_gb":8,"disk_gb":80}`
		identity := []option{withResource("instance", "abc-123"), withProject("proj-456")}
		drafts := meterResource(t, []event.Stored{
			ev("e1", "compute.instance.create.end", utc(time.February, 2), append(identity,
				withState("active"), withSize(size(t, powered)))...),
			ev("e2", "compute.instance.power_off.end", utc(time.March, 11), append(identity,
				withState("shutoff"))...),
			ev("e3", "compute.instance.power_on.end", utc(time.March, 21), append(identity,
				withState("active"))...),
		}, periodFrom, periodTo)
		if len(drafts) != 3 {
			t.Fatalf("MeterResource() = %d drafts, want 3", len(drafts))
		}

		querier := &fakeQuerier{values: map[time.Time]string{
			utc(time.March, 11): "18.0",
			utc(time.March, 21): "0",
			periodTo:            "22.5",
		}}
		m := newMeasurer(t, nil, querier, egressSource(true))

		warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: instance, Drafts: drafts}})
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
		if warnings != nil {
			t.Errorf("Apply() warnings = %#v, want none", warnings)
		}

		wantEgress := []string{"18.0000", "0.0000", "22.5000"}
		for i, d := range drafts {
			if got := quantityOf(t, d, "egress_gb"); got != wantEgress[i] {
				t.Errorf("draft %d Usage[egress_gb] = %s, want %s", i, got, wantEgress[i])
			}
		}

		// Each draft is queried over its own length, at its own end.
		wantQueries := []queryCall{
			{
				expr: `sum(increase(ceilometer_network_outgoing_bytes{cloud="os-prod-eu1", resource_id="abc-123"}[240h])) / 1e9`,
				at:   utc(time.March, 11),
			},
			{
				expr: `sum(increase(ceilometer_network_outgoing_bytes{cloud="os-prod-eu1", resource_id="abc-123"}[240h])) / 1e9`,
				at:   utc(time.March, 21),
			},
			{
				expr: `sum(increase(ceilometer_network_outgoing_bytes{cloud="os-prod-eu1", resource_id="abc-123"}[264h])) / 1e9`,
				at:   periodTo,
			},
		}
		if !reflect.DeepEqual(querier.calls, wantQueries) {
			t.Errorf("the querier saw %+v, want %+v", querier.calls, wantQueries)
		}
	})

	t.Run("an empty result is zero and a value is rounded to four places", func(t *testing.T) {
		tests := []struct {
			name, value, want string
		}{
			{name: "a resource that reported no sample", value: "0", want: "0.0000"},
			{name: "a fifth place rounded half away from zero", value: "22.49995", want: "22.5000"},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				drafts := []metering.UsageDraft{draft(periodFrom, periodTo, map[string]any{})}
				querier := &fakeQuerier{values: map[time.Time]string{periodTo: tc.value}}
				m := newMeasurer(t, nil, querier, egressSource(true))

				if _, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: instance, Drafts: drafts}}); err != nil {
					t.Fatalf("Apply() error = %v, want nil", err)
				}
				if got := quantityOf(t, drafts[0], "egress_gb"); got != tc.want {
					t.Errorf("Usage[egress_gb] = %s, want %s", got, tc.want)
				}
			})
		}
	})

	t.Run("a resource no source measures is untouched", func(t *testing.T) {
		usage := map[string]any{
			"minutes":    money.NewQuantity(money.Minutes(2678400)),
			"count":      1,
			"storage_gb": 10.0,
		}
		metered := maps.Clone(usage)
		counter := &fakeCounter{}
		querier := &fakeQuerier{}
		m := newMeasurer(t, counter, querier, egressSource(true))

		resources := []metering.ResourceUsage{
			{Resource: repository, Drafts: []metering.UsageDraft{draft(periodFrom, periodTo, usage)}},
		}
		warnings, err := m.Apply(t.Context(), resources)
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
		if warnings != nil {
			t.Errorf("Apply() warnings = %#v, want none", warnings)
		}
		if !reflect.DeepEqual(usage, metered) {
			t.Errorf("Usage = %#v, want %#v", usage, metered)
		}
		if len(counter.calls) != 0 || len(querier.calls) != 0 {
			t.Errorf("the seams saw %+v and %+v, want no call", counter.calls, querier.calls)
		}
	})

	t.Run("nothing to measure calls neither seam", func(t *testing.T) {
		tests := []struct {
			name      string
			resources []metering.ResourceUsage
		}{
			{name: "no resources at all"},
			{name: "an empty resource list", resources: []metering.ResourceUsage{}},
			{name: "a resource without drafts", resources: []metering.ResourceUsage{{Resource: instance}}},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				counter := &fakeCounter{}
				querier := &fakeQuerier{}
				m := newMeasurer(t, counter, querier, egressSource(true))

				warnings, err := m.Apply(t.Context(), tc.resources)
				if err != nil {
					t.Fatalf("Apply() error = %v, want nil", err)
				}
				if warnings != nil {
					t.Errorf("Apply() warnings = %#v, want none", warnings)
				}
				if len(counter.calls) != 0 || len(querier.calls) != 0 {
					t.Errorf("the seams saw %+v and %+v, want no call", counter.calls, querier.calls)
				}
			})
		}
	})

	t.Run("a draft without a usage object gets one", func(t *testing.T) {
		counter := &fakeCounter{counts: map[countKey]int64{
			{eventType: "repository.pull", from: periodFrom}: 5,
		}}
		m := newMeasurer(t, counter, nil, pullsSource())

		resources := []metering.ResourceUsage{
			{Resource: repository, Drafts: []metering.UsageDraft{draft(periodFrom, periodTo, nil)}},
		}
		if _, err := m.Apply(t.Context(), resources); err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
		if got := quantityOf(t, resources[0].Drafts[0], "pulls"); got != "5.0000" {
			t.Errorf("Usage[pulls] = %s, want 5.0000", got)
		}
	})

	t.Run("a metric colliding with a size field fails the pass", func(t *testing.T) {
		tests := []struct {
			name     string
			required bool
		}{
			{name: "a required source", required: true},
			{name: "an optional source", required: false},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				drafts := []metering.UsageDraft{
					draft(periodFrom, utc(time.March, 11), map[string]any{"egress_gb": 3.0}),
					draft(utc(time.March, 11), periodTo, map[string]any{}),
				}
				querier := &fakeQuerier{values: map[time.Time]string{periodTo: "22.5"}}
				m := newMeasurer(t, nil, querier, egressSource(tc.required))

				warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: instance, Drafts: drafts}})
				if err == nil {
					t.Fatal("Apply() error = nil, want an error")
				}
				want := `already carries "egress_gb", which the counter source for openstack/instance would overwrite`
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Apply() error = %q, want it to contain %q", err, want)
				}
				if warnings != nil {
					t.Errorf("Apply() warnings = %#v, want none", warnings)
				}
				// The collision is found before the first draft is queried, and
				// the pass ends there, so the second draft is not measured.
				if len(querier.calls) != 0 {
					t.Errorf("the querier saw %+v, want no call", querier.calls)
				}
			})
		}
	})

	t.Run("a required metricsql source that fails ends the pass", func(t *testing.T) {
		sentinel := errors.New("boom")
		drafts := []metering.UsageDraft{
			draft(periodFrom, utc(time.March, 11), map[string]any{}),
			draft(utc(time.March, 11), periodTo, map[string]any{}),
		}
		querier := &fakeQuerier{err: sentinel}
		m := newMeasurer(t, nil, querier, egressSource(true))

		warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: instance, Drafts: drafts}})
		if err == nil {
			t.Fatal("Apply() error = nil, want an error")
		}
		want := "measuring egress_gb of os-prod-eu1/instance/abc-123 over " +
			"[2026-03-01T00:00:00Z, 2026-03-11T00:00:00Z):"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply() error = %q, want it to contain %q", err, want)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("Apply() error = %v, want it to wrap the querier's error", err)
		}
		if warnings != nil {
			t.Errorf("Apply() warnings = %#v, want none", warnings)
		}
	})

	t.Run("an optional metricsql source that fails is a warning", func(t *testing.T) {
		sentinel := errors.New("boom")
		drafts := []metering.UsageDraft{
			draft(periodFrom, utc(time.March, 11), map[string]any{}),
			draft(utc(time.March, 11), periodTo, map[string]any{}),
		}
		failAt := utc(time.March, 11)
		querier := &fakeQuerier{
			values: map[time.Time]string{periodTo: "22.5"},
			err:    sentinel,
			failAt: &failAt,
		}
		m := newMeasurer(t, nil, querier, egressSource(false))

		warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: instance, Drafts: drafts}})
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
		if len(warnings) != 1 {
			t.Fatalf("Apply() = %d warnings, want 1", len(warnings))
		}

		got := warnings[0]
		named := got
		named.Detail = ""
		want := counters.Warning{
			Cloud: "os-prod-eu1", ResourceType: "instance", ResourceID: "abc-123",
			Metric: "egress_gb", FromTS: drafts[0].FromTS, ToTS: drafts[0].ToTS,
			Code: "counter_source_failed",
		}
		if named != want {
			t.Errorf("Apply() warning = %+v, want %+v", named, want)
		}
		for _, part := range []string{"measuring egress_gb of", "boom"} {
			if !strings.Contains(got.Detail, part) {
				t.Errorf("Detail = %q, want it to contain %q", got.Detail, part)
			}
		}

		// The draft the query failed for is billed without the metric, the one
		// after it carries its value.
		if _, taken := drafts[0].Usage["egress_gb"]; taken {
			t.Errorf("draft 0 Usage[egress_gb] = %#v, want it absent", drafts[0].Usage["egress_gb"])
		}
		if got := quantityOf(t, drafts[1], "egress_gb"); got != "22.5000" {
			t.Errorf("draft 1 Usage[egress_gb] = %s, want 22.5000", got)
		}

		encoded, err := json.Marshal(warnings[0])
		if err != nil {
			t.Fatalf("Marshal() error = %v, want nil", err)
		}
		for _, key := range []string{
			`"cloud":`, `"resource_type":`, `"resource_id":`, `"metric":`,
			`"from_ts":`, `"to_ts":`, `"code":`, `"detail":`,
		} {
			if !strings.Contains(string(encoded), key) {
				t.Errorf("Marshal(warning) = %s, want it to hold %s", encoded, key)
			}
		}
	})

	t.Run("a count that fails ends the pass", func(t *testing.T) {
		sentinel := errors.New("tx aborted")
		drafts := []metering.UsageDraft{draft(periodFrom, utc(time.March, 18), map[string]any{})}
		counter := &fakeCounter{err: sentinel}
		// The source is not required and the pass ends anyway: a failed
		// statement leaves the snapshot's transaction aborted.
		m := newMeasurer(t, counter, nil, pullsSource())

		warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: repository, Drafts: drafts}})
		if err == nil {
			t.Fatal("Apply() error = nil, want an error")
		}
		want := "measuring pulls of harbor-prod/repository/team-alpha/app over " +
			"[2026-03-01T00:00:00Z, 2026-03-18T00:00:00Z):"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply() error = %q, want it to contain %q", err, want)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("Apply() error = %v, want it to wrap the counter's error", err)
		}
		if warnings != nil {
			t.Errorf("Apply() warnings = %#v, want none", warnings)
		}
	})

	// The sources of a resource are looked up by the platform and the resource
	// type together, so a second resource type of the same platform is measured
	// with its own sources or with none.
	t.Run("a second resource type of the same platform is not measured with the first's sources",
		func(t *testing.T) {
			volume := source.Resource{
				Cloud: "os-prod-eu1", Platform: "openstack",
				ResourceType: "volume", ResourceID: "vol-9",
			}
			instanceDrafts := []metering.UsageDraft{draft(periodFrom, periodTo, map[string]any{})}
			volumeDrafts := []metering.UsageDraft{draft(periodFrom, periodTo, map[string]any{})}

			querier := &fakeQuerier{values: map[time.Time]string{periodTo: "22.5"}}
			m := newMeasurer(t, nil, querier, egressSource(true))

			warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{
				{Resource: instance, Drafts: instanceDrafts},
				{Resource: volume, Drafts: volumeDrafts},
			})
			if err != nil {
				t.Fatalf("Apply() error = %v, want nil", err)
			}
			if warnings != nil {
				t.Errorf("Apply() warnings = %#v, want none", warnings)
			}

			if got := quantityOf(t, instanceDrafts[0], "egress_gb"); got != "22.5000" {
				t.Errorf("the instance draft Usage[egress_gb] = %s, want 22.5000", got)
			}
			if len(volumeDrafts[0].Usage) != 0 {
				t.Errorf("the volume draft Usage = %#v, want it untouched", volumeDrafts[0].Usage)
			}

			// One call, for the instance: the volume's drafts are never
			// measured with the instance's query.
			wantQueries := []queryCall{{
				expr: `sum(increase(ceilometer_network_outgoing_bytes{cloud="os-prod-eu1", ` +
					`resource_id="abc-123"}[744h])) / 1e9`,
				at: periodTo,
			}}
			if !reflect.DeepEqual(querier.calls, wantQueries) {
				t.Errorf("the querier saw %+v, want %+v", querier.calls, wantQueries)
			}
		})

	// A store that is down fails every draft the same way, and each failure
	// costs the client's whole retry ladder. The pass stops querying such a
	// source rather than paying that ladder once per draft.
	t.Run("an optional source that keeps failing is not queried for every draft", func(t *testing.T) {
		sentinel := errors.New("boom")
		drafts := make([]metering.UsageDraft, 0, 9)
		for day := 1; day <= 9; day++ {
			drafts = append(drafts, draft(utc(time.March, day), utc(time.March, day+1), map[string]any{}))
		}
		querier := &fakeQuerier{err: sentinel}
		m := newMeasurer(t, nil, querier, egressSource(false))

		warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: instance, Drafts: drafts}})
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}

		// Every draft is billed without the metric and says so, but only the
		// first five reached the store.
		if len(warnings) != len(drafts) {
			t.Fatalf("Apply() = %d warnings, want %d", len(warnings), len(drafts))
		}
		if len(querier.calls) != 5 {
			t.Errorf("the querier saw %d calls, want 5", len(querier.calls))
		}
		for i, w := range warnings {
			if w.Code != "counter_source_failed" || w.Metric != "egress_gb" {
				t.Errorf("warning %d = %+v, want a counter_source_failed of egress_gb", i, w)
			}
			if !w.FromTS.Equal(drafts[i].FromTS) {
				t.Errorf("warning %d FromTS = %s, want %s", i, w.FromTS, drafts[i].FromTS)
			}
		}
		want := "the source failed 5 drafts in a row, so it is queried again only every 50 drafts"
		if !strings.Contains(warnings[5].Detail, want) {
			t.Errorf("warning 5 Detail = %q, want it to contain %q", warnings[5].Detail, want)
		}
		for _, d := range drafts {
			if _, taken := d.Usage["egress_gb"]; taken {
				t.Errorf("Usage[egress_gb] = %#v, want it absent", d.Usage["egress_gb"])
			}
		}
	})

	// Idle resources are ordinary in a billing period, and a source computing a
	// ratio divides by zero for each of them: MetricsQL prints NaN, which does
	// not parse. Counting those against the source would back it off, and the
	// active resources metered after them would lose the billed metric to a
	// store that is healthy and answering.
	t.Run("an answer one resource shaped does not stop the source for the drafts after it",
		func(t *testing.T) {
			drafts := make([]metering.UsageDraft, 0, 9)
			for day := 1; day <= 9; day++ {
				drafts = append(drafts, draft(utc(time.March, day), utc(time.March, day+1), map[string]any{}))
			}
			querier := &fakeQuerier{
				err: fmt.Errorf("%w: parsing the VictoriaMetrics value %q: boom",
					counters.ErrAnswerShape, "NaN"),
			}
			m := newMeasurer(t, nil, querier, egressSource(false))

			warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: instance, Drafts: drafts}})
			if err != nil {
				t.Fatalf("Apply() error = %v, want nil", err)
			}

			// Every draft is billed without the metric, and every one of them
			// reached the store: none of these failures was the store's.
			if len(warnings) != len(drafts) {
				t.Fatalf("Apply() = %d warnings, want %d", len(warnings), len(drafts))
			}
			if len(querier.calls) != len(drafts) {
				t.Errorf("the querier saw %d calls, want %d", len(querier.calls), len(drafts))
			}
			for i, w := range warnings {
				if w.Code != "counter_source_failed" {
					t.Errorf("warning %d Code = %q, want counter_source_failed", i, w.Code)
				}
				if part := "the source failed"; strings.Contains(w.Detail, part) {
					t.Errorf("warning %d Detail = %q, want it not to blame the store", i, w.Detail)
				}
			}
		})

	t.Run("a draft the store answers again resets the count of failures", func(t *testing.T) {
		drafts := make([]metering.UsageDraft, 0, 10)
		for day := 1; day <= 10; day++ {
			drafts = append(drafts, draft(utc(time.March, day), utc(time.March, day+1), map[string]any{}))
		}
		// The querier answers the fifth draft and fails every other, so the
		// four failures before it do not add up with the five after it and
		// every draft is queried.
		querier := &fakeQuerier{values: map[time.Time]string{drafts[4].ToTS: "22.5"}}
		m := newMeasurer(t, nil, querier, egressSource(false))

		warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: instance, Drafts: drafts}})
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
		if len(warnings) != 9 {
			t.Fatalf("Apply() = %d warnings, want 9", len(warnings))
		}
		if len(querier.calls) != len(drafts) {
			t.Errorf("the querier saw %d calls, want %d", len(querier.calls), len(drafts))
		}
		if got := quantityOf(t, drafts[4], "egress_gb"); got != "22.5000" {
			t.Errorf("draft 4 Usage[egress_gb] = %s, want 22.5000", got)
		}
	})

	// A store that is down for part of the pass comes back during it, and the
	// source has to be queried again to be found: the drafts after the outage
	// are billed with the metric rather than warned to the end of the pass.
	t.Run("a source the store stopped answering is queried again later in the pass", func(t *testing.T) {
		const drafted = 55
		drafts := make([]metering.UsageDraft, 0, drafted)
		for h := range drafted {
			from := periodFrom.Add(time.Duration(h) * time.Hour)
			drafts = append(drafts, draft(from, from.Add(time.Hour), map[string]any{}))
		}
		// The store answers from the 51st draft on, which is the one the probe
		// falls on: the five failures before it stop the queries and the 45
		// drafts between are left out without one.
		values := make(map[time.Time]string, drafted-50)
		for _, d := range drafts[50:] {
			values[d.ToTS] = "22.5"
		}
		querier := &fakeQuerier{values: values}
		m := newMeasurer(t, nil, querier, egressSource(false))

		warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{{Resource: instance, Drafts: drafts}})
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
		// The five drafts the store failed and the 45 it was not asked about.
		if len(warnings) != 50 {
			t.Fatalf("Apply() = %d warnings, want 50", len(warnings))
		}
		// Those five queries, the probe, and every draft after it.
		if len(querier.calls) != 10 {
			t.Errorf("the querier saw %d calls, want 10", len(querier.calls))
		}
		for i, d := range drafts[50:] {
			if got := quantityOf(t, d, "egress_gb"); got != "22.5000" {
				t.Errorf("draft %d Usage[egress_gb] = %s, want 22.5000", 50+i, got)
			}
		}
	})

	// The identity values come from ingested event data, so a value that would
	// compose a query of its own is refused rather than escaped into the
	// matcher. Nothing upstream bounds the characters such a value may hold,
	// and the refusal is the same however often the pass is rerun, so failing
	// the pass over it would leave the period unclosable.
	// The identity comes from ingested event data, so warning over it for a
	// required source would let whoever writes an event decide whether a
	// revenue-relevant counter is measured: a create event whose resource_id
	// holds a space passes event.Validate and closes the period unmeasured.
	t.Run("an identity a query may not carry ends the pass for a required source", func(t *testing.T) {
		injected := source.Resource{
			Cloud: "os-prod-eu1", Platform: "openstack", ResourceType: "instance",
			ResourceID: "vm münchen 01",
		}
		injectedDrafts := []metering.UsageDraft{draft(periodFrom, periodTo, map[string]any{})}
		querier := &fakeQuerier{values: map[time.Time]string{periodTo: "22.5"}}
		m := newMeasurer(t, nil, querier, egressSource(true))

		warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{
			{Resource: injected, Drafts: injectedDrafts},
		})
		if err == nil {
			t.Fatal("Apply() error = nil, want an error")
		}
		for _, part := range []string{
			"measuring egress_gb of os-prod-eu1/instance/vm münchen 01",
			"holds a character a MetricsQL query may not carry",
		} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("Apply() error = %q, want it to contain %q", err, part)
			}
		}
		if warnings != nil {
			t.Errorf("Apply() warnings = %#v, want none", warnings)
		}
		if len(querier.calls) != 0 {
			t.Errorf("the querier saw %+v, want no call", querier.calls)
		}
	})

	t.Run("an identity a query may not carry warns an optional source under its own code",
		func(t *testing.T) {
			injected := source.Resource{
				Cloud: "os-prod-eu1", Platform: "openstack", ResourceType: "instance",
				ResourceID: `x"}[1s])) or vector(1e12) #`,
			}
			injectedDrafts := []metering.UsageDraft{draft(periodFrom, periodTo, map[string]any{})}
			cleanDrafts := []metering.UsageDraft{draft(periodFrom, periodTo, map[string]any{})}
			querier := &fakeQuerier{values: map[time.Time]string{periodTo: "22.5"}}
			m := newMeasurer(t, nil, querier, egressSource(false))

			warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{
				{Resource: injected, Drafts: injectedDrafts},
				{Resource: instance, Drafts: cleanDrafts},
			})
			if err != nil {
				t.Fatalf("Apply() error = %v, want nil", err)
			}
			if len(warnings) != 1 {
				t.Fatalf("Apply() = %d warnings, want 1", len(warnings))
			}

			got := warnings[0]
			named := got
			named.Detail = ""
			// The code is not the one a store that was down yields: this
			// warning names a resource to fix, and no rerun clears it.
			want := counters.Warning{
				Cloud: "os-prod-eu1", ResourceType: "instance", ResourceID: injected.ResourceID,
				Metric: "egress_gb", FromTS: periodFrom, ToTS: periodTo,
				Code: "counter_identity_not_queryable",
			}
			if named != want {
				t.Errorf("Apply() warning = %+v, want %+v", named, want)
			}
			if part := "holds a character a MetricsQL query may not carry"; !strings.Contains(got.Detail, part) {
				t.Errorf("Detail = %q, want it to contain %q", got.Detail, part)
			}
			if _, taken := injectedDrafts[0].Usage["egress_gb"]; taken {
				t.Errorf("the refused draft Usage[egress_gb] = %#v, want it absent",
					injectedDrafts[0].Usage["egress_gb"])
			}

			// The resource whose identity renders is billed, and it is the only
			// one the store is asked about.
			if got := quantityOf(t, cleanDrafts[0], "egress_gb"); got != "22.5000" {
				t.Errorf("the instance draft Usage[egress_gb] = %s, want 22.5000", got)
			}
			if len(querier.calls) != 1 {
				t.Errorf("the querier saw %+v, want one call", querier.calls)
			}
		})

	// A refusal is not a failure of the store: counting it against the source
	// would leave the metric out of every resource measured after a handful of
	// them, which is a resource id away from dropping a billed metric from the
	// whole deployment.
	t.Run("an identity a query may not carry does not stop the source for the next resource",
		func(t *testing.T) {
			injected := source.Resource{
				Cloud: "os-prod-eu1", Platform: "openstack", ResourceType: "instance",
				ResourceID: "vm münchen 01",
			}
			injectedDrafts := make([]metering.UsageDraft, 0, 6)
			for day := 1; day <= 6; day++ {
				injectedDrafts = append(injectedDrafts,
					draft(utc(time.March, day), utc(time.March, day+1), map[string]any{}))
			}
			cleanDrafts := []metering.UsageDraft{draft(periodFrom, periodTo, map[string]any{})}
			querier := &fakeQuerier{values: map[time.Time]string{periodTo: "22.5"}}
			m := newMeasurer(t, nil, querier, egressSource(false))

			warnings, err := m.Apply(t.Context(), []metering.ResourceUsage{
				{Resource: injected, Drafts: injectedDrafts},
				{Resource: instance, Drafts: cleanDrafts},
			})
			if err != nil {
				t.Fatalf("Apply() error = %v, want nil", err)
			}
			// One per refused draft, and none for the resource after them.
			if len(warnings) != len(injectedDrafts) {
				t.Fatalf("Apply() = %d warnings, want %d", len(warnings), len(injectedDrafts))
			}
			if got := quantityOf(t, cleanDrafts[0], "egress_gb"); got != "22.5000" {
				t.Errorf("the instance draft Usage[egress_gb] = %s, want 22.5000", got)
			}
			if len(querier.calls) != 1 {
				t.Errorf("the querier saw %+v, want one call", querier.calls)
			}
		})

	t.Run("a canceled run fails even an optional source", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		drafts := []metering.UsageDraft{draft(periodFrom, periodTo, map[string]any{})}
		querier := &fakeQuerier{values: map[time.Time]string{periodTo: "22.5"}}
		m := newMeasurer(t, nil, querier, egressSource(false))

		warnings, err := m.Apply(ctx, []metering.ResourceUsage{{Resource: instance, Drafts: drafts}})
		if err == nil {
			t.Fatal("Apply() error = nil, want an error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Apply() error = %v, want it to wrap context.Canceled", err)
		}
		if warnings != nil {
			t.Errorf("Apply() warnings = %#v, want none", warnings)
		}
	})
}
