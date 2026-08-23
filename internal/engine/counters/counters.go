// Package counters measures the counter metrics of a deployment per usage
// draft, from the counter sources file the deployment configures. The pass sits
// after metering.Meter and before anything is persisted, while the reporting
// snapshot is still open: the events reads go through the same snapshot as the
// history the drafts were folded from, so a counter and the drafts it slices
// into see the same data.
//
// A source measures its metric in one of two ways. An events source counts the
// events of one type inside the draft's interval. A metricsql source runs an
// instant query against VictoriaMetrics at the draft's end, over the draft's
// length.
//
// A metricsql source is required unless the file says required: false. A failed
// query of a required source fails the pass, because billing data must not
// silently omit a revenue-relevant counter. A failed query of an optional
// source leaves the metric out of that draft and yields a warning.
//
// This package persists nothing.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.4
// and decision D7.
package counters

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"
)

// Kind is how a counter source measures its metric.
type Kind string

const (
	// KindEvents counts the events of one type inside the draft's interval.
	KindEvents Kind = "events"
	// KindMetricsQL runs an instant MetricsQL query against VictoriaMetrics at
	// the draft's end.
	KindMetricsQL Kind = "metricsql"
)

// The two usage keys the engine derives itself, held by usageMinutes and
// usageCount in internal/engine/metering/metering.go. A counter may not claim
// them: its value would replace a quantity the drafts already carry.
var reservedMetrics = []string{"minutes", "count"}

// The placeholders a metricsql query may use, and the pattern that finds them.
// A label selector such as {cloud="{cloud}"} does not match the pattern; only
// the placeholder inside it does.
//
// identityPlaceholder finds the three placeholders whose value a query may not
// read as a pattern. regexMatchedIdentity finds a label matcher that reads its
// value as one: =~ or !~ and the whole string literal behind it, in each of the
// three forms MetricsQL writes a literal in. The literal is matched whole
// rather than up to the placeholder, so a quantifier such as [0-9]{1,3} before
// it cannot hide it behind a brace. regexArgumentFunc finds the MetricsQL
// functions that read a bare string argument as a pattern, which carry no =~
// for the matcher pattern to find.
var (
	placeholderPattern   = regexp.MustCompile(`\{([a-z_]+)\}`)
	knownPlaceholders    = []string{"cloud", "resource_id", "project_id", "window"}
	identityPlaceholder  = regexp.MustCompile(`\{(cloud|resource_id|project_id)\}`)
	regexMatchedIdentity = regexp.MustCompile(
		"[=!]~\\s*(?:\"(?:[^\"\\\\]|\\\\.)*\"|'(?:[^'\\\\]|\\\\.)*'|`[^`]*`)")
	regexArgumentFunc = regexp.MustCompile(
		`\b(label_replace|label_transform|label_match|label_mismatch)\s*\(`)
)

// Source is one resolved entry of the counter sources file: which metric of
// which platform and resource type it measures, and how. EventType is set for
// events sources and Query for metricsql sources. Required is true unless the
// file said required: false, and it applies to metricsql sources only.
type Source struct {
	Platform, ResourceType, Metric string
	Kind                           Kind
	EventType                      string
	Query                          string
	Required                       bool
}

// Config is the counter sources of a deployment, in file order.
type Config struct {
	Sources []Source
}

// HasMetricsQL reports whether any source is a metricsql source, which is how
// a caller knows whether it needs a VictoriaMetrics client at all.
func (c Config) HasMetricsQL() bool {
	for _, s := range c.Sources {
		if s.Kind == KindMetricsQL {
			return true
		}
	}
	return false
}

// sourcesFile is the counter sources file as it is written.
type sourcesFile struct {
	Sources []sourceFile `yaml:"sources"`
}

// sourceFile is one entry of that file. Required is a pointer so that an absent
// key can be told from required: false.
type sourceFile struct {
	Platform     string `yaml:"platform"`
	ResourceType string `yaml:"resource_type"`
	Metric       string `yaml:"metric"`
	Kind         string `yaml:"kind"`
	EventType    string `yaml:"event_type"`
	Query        string `yaml:"query"`
	Required     *bool  `yaml:"required"`
}

// Load reads the counter sources at path. An empty path yields the zero Config
// and no error, which is what a deployment that measures no counter metric
// configures.
//
// A path that cannot be read is an error rather than a run without counters: a
// misconfigured path would otherwise drop a billed metric from every draft of
// the period without anything saying so.
func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading the counter sources %s: %w", path, err)
	}

	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes the counter sources and checks every entry. An empty document,
// one that holds only comments, and one without sources each yield the zero
// Config and no error.
//
// A key the file form does not know is an error naming it, so that a misspelled
// event_typ fails the run instead of leaving the source measuring nothing.
func Parse(data []byte) (Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var file sourcesFile
	if err := dec.Decode(&file); err != nil {
		// A document with no content at all yields no value to decode into.
		if errors.Is(err, io.EOF) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("parsing the counter sources: %w", err)
	}

	// Only the first document is decoded. A second one, appended to add another
	// team's counters, would otherwise leave its sources out of every draft of
	// the period without anything saying so.
	var extra sourcesFile
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return Config{}, fmt.Errorf("parsing the counter sources: %w", err)
	default:
		return Config{}, errors.New(
			"parsing the counter sources: the file holds more than one document, every source belongs in the first")
	}

	var sources []Source
	for _, entry := range file.Sources {
		sources = append(sources, Source{
			Platform:     entry.Platform,
			ResourceType: entry.ResourceType,
			Metric:       entry.Metric,
			Kind:         Kind(entry.Kind),
			EventType:    entry.EventType,
			Query:        entry.Query,
			Required:     entry.Required == nil || *entry.Required,
		})
	}

	if err := validate(sources); err != nil {
		return Config{}, err
	}

	// Only the file form tells required: false from an absent key, so the rule
	// about the key's presence is checked here rather than in validate.
	for i, entry := range file.Sources {
		if Kind(entry.Kind) == KindEvents && entry.Required != nil {
			return Config{}, fmt.Errorf("sources[%d]: required applies to metricsql sources only", i)
		}
	}

	return Config{Sources: sources}, nil
}

// validate reports the first source that does not hold, named by its position
// in the file. It takes the sources rather than a Config so that a hand-built
// configuration is checked the same way a loaded one is.
func validate(sources []Source) error {
	type key struct {
		platform, resourceType, metric string
	}
	seen := make(map[key]bool, len(sources))

	for i, s := range sources {
		entry := fmt.Sprintf("sources[%d]", i)

		if s.Platform == "" {
			return fmt.Errorf("%s: platform must be set", entry)
		}
		if s.ResourceType == "" {
			return fmt.Errorf("%s: resource_type must be set", entry)
		}
		if s.Metric == "" {
			return fmt.Errorf("%s: metric must be set", entry)
		}
		if slices.Contains(reservedMetrics, s.Metric) {
			return fmt.Errorf("%s: metric %q is reserved by the engine", entry, s.Metric)
		}

		switch s.Kind {
		case KindEvents:
			if s.EventType == "" {
				return fmt.Errorf("%s: event_type must be set", entry)
			}
			if s.Query != "" {
				return fmt.Errorf("%s: query applies to metricsql sources only", entry)
			}
		case KindMetricsQL:
			if s.Query == "" {
				return fmt.Errorf("%s: query must be set", entry)
			}
			if s.EventType != "" {
				return fmt.Errorf("%s: event_type applies to events sources only", entry)
			}
			for _, match := range placeholderPattern.FindAllStringSubmatch(s.Query, -1) {
				if !slices.Contains(knownPlaceholders, match[1]) {
					return fmt.Errorf("%s: query uses the unknown placeholder {%s}", entry, match[1])
				}
			}
			// An identity value is inert as a literal, not as a pattern: a
			// regex matcher reads the dot such a value may hold as a wildcard,
			// so the matcher would select the series of the resources beside
			// the one it names and an aggregate over them would bill this
			// resource for theirs. Every matcher of the query is looked at, not
			// only the first: one that reads its own literal is no reason to
			// stop before the one that reads an identity.
			for _, matcher := range regexMatchedIdentity.FindAllString(s.Query, -1) {
				if id := identityPlaceholder.FindStringSubmatch(matcher); id != nil {
					return fmt.Errorf(
						"%s: query matches {%s} with =~ or !~, which reads the substituted value as a pattern; "+
							"an identity is matched with = or !=", entry, id[1])
				}
			}
			// The same hazard without a matcher to mark it: these functions
			// read one of their string arguments as a pattern. Which argument a
			// placeholder sits in cannot be told apart from here without
			// parsing MetricsQL, so a query that calls one of them and
			// substitutes an identity anywhere is refused.
			if fn := regexArgumentFunc.FindStringSubmatch(s.Query); fn != nil &&
				identityPlaceholder.MatchString(s.Query) {
				return fmt.Errorf(
					"%s: query calls %s, whose pattern argument would read a substituted identity as a pattern",
					entry, fn[1])
			}
		default:
			return fmt.Errorf("%s: kind %q must be events or metricsql", entry, s.Kind)
		}

		// Two entries for one metric of one resource type would write the same
		// usage key twice, and the second value would be the one billed.
		k := key{platform: s.Platform, resourceType: s.ResourceType, metric: s.Metric}
		if seen[k] {
			return fmt.Errorf("%s: %s of %s/%s is configured twice", entry, s.Metric, s.Platform, s.ResourceType)
		}
		seen[k] = true
	}

	return nil
}
