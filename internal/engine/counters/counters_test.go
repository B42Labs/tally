package counters_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b42labs/tally/internal/engine/counters"
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
