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
// query of a required source fails the pass, whatever failed it, because
// billing data must not silently omit a revenue-relevant counter: an identity
// no MetricsQL query may carry would otherwise leave that omission to whoever
// writes an event. A failed query of an optional source leaves the metric out
// of that draft and yields a warning. So does an identity no query may carry,
// under its own code, because unlike a failed query it does not clear on a
// rerun.
//
// This package persists nothing.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.4
// and decision D7.
package counters

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/source"
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

// EventCounter counts the events of one type inside an interval. It is the seam
// an events source is measured through, implemented by *source.Snapshot; the
// unit tests here and the engine's golden suite substitute their own.
type EventCounter interface {
	CountEvents(ctx context.Context, r source.Resource, eventType string, from, to time.Time) (int64, error)
}

var _ EventCounter = (*source.Snapshot)(nil)

// Querier runs an instant query and returns its single value. It is the seam a
// metricsql source is measured through, implemented by *VMClient; the unit tests
// here and the engine's golden suite substitute their own.
type Querier interface {
	Query(ctx context.Context, expr string, at time.Time) (decimal.Decimal, error)
}

var _ Querier = (*VMClient)(nil)

// WarningCounterSourceFailed marks a metricsql source declared required: false
// whose query failed for one draft. The metric is left out of that draft.
const WarningCounterSourceFailed = "counter_source_failed"

// WarningCounterIdentityNotQueryable marks a resource whose identity a
// MetricsQL query may not carry, for a source declared required: false. It is a
// second code rather than the one above because it does not clear on a rerun:
// the metric stays missing from that resource's drafts until the identity
// changes, which is a different thing to alert on than a store that was down.
const WarningCounterIdentityNotQueryable = "counter_identity_not_queryable"

// Warning names a metric that is missing from one draft and why. It is a second
// warning type beside metering.Warning because it names a metric and an
// interval, which a metering warning has no field for; the run writes both lists
// to its stats. Detail is the failed source's error text.
type Warning struct {
	Cloud        string    `json:"cloud"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	Metric       string    `json:"metric"`
	FromTS       time.Time `json:"from_ts"`
	ToTS         time.Time `json:"to_ts"`
	Code         string    `json:"code"`
	Detail       string    `json:"detail"`
}

// maxSourceFailures is how many drafts in a row one optional metricsql source
// may fail before Apply stops querying it. A store that is down fails every
// draft the same way and each failure costs the client's whole retry ladder, so
// querying it again for every remaining draft would stretch the pass by minutes
// per draft while the reporting snapshot stays open.
const maxSourceFailures = 5

// probeEvery is how often a source past maxSourceFailures is queried again
// anyway: on every probeEvery-th draft it is left out of. A store that came
// back mid-pass is found before the pass ends rather than only at the next
// run, without paying the retry ladder for the drafts between.
const probeEvery = 50

// errNotQueryable marks the failure of a metricsql source that is a property of
// the resource rather than of its answer or of the store: an identity
// RenderQuery refuses to substitute. Apply warns an optional source under its
// own code for it, because no rerun renders it differently.
var errNotQueryable = errors.New("the resource cannot be queried")

// Measurer measures the counter metrics of a configuration into usage drafts.
type Measurer struct {
	events EventCounter
	vm     Querier
	// byType is the sources of each platform and resource type, in file order.
	byType map[resourceTypeKey][]Source
}

// resourceTypeKey is the platform and resource type a set of sources measures.
type resourceTypeKey struct{ platform, resourceType string }

// New checks cfg the way a loaded file is checked and indexes its sources by the
// resource type they measure, so a hand-built configuration is held to the same
// rules as one Load returned.
//
// A kind configured without the seam it is measured through is an error rather
// than a run that leaves those metrics out of every draft. A configuration
// without sources needs neither seam.
func New(cfg Config, events EventCounter, vm Querier) (*Measurer, error) {
	if err := validate(cfg.Sources); err != nil {
		return nil, err
	}

	var eventSources, metricsQLSources int
	byType := make(map[resourceTypeKey][]Source)
	for _, s := range cfg.Sources {
		// validate leaves no third kind, so anything but an events source is
		// measured with a query.
		if s.Kind == KindEvents {
			eventSources++
		} else {
			metricsQLSources++
		}
		k := resourceTypeKey{platform: s.Platform, resourceType: s.ResourceType}
		byType[k] = append(byType[k], s)
	}

	if eventSources > 0 && events == nil {
		return nil, fmt.Errorf("%d events sources are configured but no event counter was given", eventSources)
	}
	if metricsQLSources > 0 && vm == nil {
		return nil, fmt.Errorf("%d metricsql sources are configured but no VictoriaMetrics querier was given",
			metricsQLSources)
	}

	return &Measurer{events: events, vm: vm, byType: byType}, nil
}

// Apply measures the counter metrics of every resource it holds sources for into
// that resource's drafts. It runs over the resources of a metering pass, after
// metering.Meter and before anything is persisted, while the reporting snapshot
// is still open.
//
// The values are written into each draft's usage object in place. That map is
// the one metering built and every copy of the draft shares it, so the caller's
// metering.Result carries the counters once Apply returns.
//
// A source that fails where its metric may not be missing ends the pass. The
// drafts measured before it keep the values they were given, which is why a
// caller discards the whole result on an error instead of persisting what came
// back. A failed optional metricsql source yields a warning instead, and its
// metric is left out of that one draft; once it has failed maxSourceFailures
// drafts in a row it is left out of the remaining ones without being queried
// again, each of them still named by its own warning, and every probeEvery-th
// of those drafts queries it once more so that a store which came back is found
// while the pass still runs.
//
// A resource whose identity a MetricsQL query may not carry fails a required
// source like any other failure: the identity comes from ingested event data,
// so warning over it instead would hand whoever writes an event the decision
// whether a billed counter is measured. An optional source is warned under
// WarningCounterIdentityNotQueryable, which unlike a failed query does not
// clear on a rerun. Neither that refusal nor an answer this one resource shaped
// is counted against the source: only a store that is down says anything about
// the next draft, so the resources whose own answers are fine keep the metric.
func (m *Measurer) Apply(ctx context.Context, resources []metering.ResourceUsage) ([]Warning, error) {
	var warnings []Warning
	// The drafts each optional metricsql source has been left out of in a
	// row, the ones it failed and the ones it was no longer queried for, reset
	// wherever one succeeds.
	failures := make(map[Source]int)

	for i := range resources {
		ru := &resources[i]
		sources := m.byType[resourceTypeKey{platform: ru.Resource.Platform, resourceType: ru.Resource.ResourceType}]
		// A resource no source measures is left as metering folded it, and
		// neither seam is called for it.
		if len(sources) == 0 {
			continue
		}

		for j := range ru.Drafts {
			d := &ru.Drafts[j]
			// Every draft metering builds carries a usage object; one built by
			// hand may not.
			if d.Usage == nil {
				d.Usage = make(map[string]any, len(sources))
			}

			for _, s := range sources {
				// A metric that would replace a size field the provider
				// reported is a configuration conflict one rename fixes,
				// whatever required says, so it fails the pass rather than
				// billing whichever value was written last.
				if _, taken := d.Usage[s.Metric]; taken {
					return nil, fmt.Errorf(
						"the usage of %s/%s/%s over [%s, %s) already carries %q, which the counter source for %s/%s would overwrite",
						ru.Resource.Cloud, ru.Resource.ResourceType, ru.Resource.ResourceID,
						d.FromTS.Format(time.RFC3339Nano), d.ToTS.Format(time.RFC3339Nano),
						s.Metric, s.Platform, s.ResourceType)
				}

				// A source that has failed the last drafts in a row is left
				// out of the remaining ones rather than queried for each of
				// them: what fails them is the store, not the draft. Every
				// probeEvery-th of them is queried anyway, so a store that
				// came back is found while the pass still runs.
				if n := failures[s]; n >= maxSourceFailures && n%probeEvery != 0 {
					failures[s] = n + 1
					warnings = append(warnings, warningOf(ru.Resource, d, s.Metric,
						WarningCounterSourceFailed, fmt.Sprintf(
							"%s: the source failed %d drafts in a row, so it is queried again only every %d drafts",
							measuring(ru.Resource, d, s.Metric), maxSourceFailures, probeEvery)))
					continue
				}

				value, err := m.measure(ctx, ru.Resource, d, s)
				if err != nil {
					wrapped := fmt.Errorf("%s: %w", measuring(ru.Resource, d, s.Metric), err)

					// A failed count aborts the snapshot's transaction, so
					// nothing after it could be read anyway; a required metric
					// may not be missing from a draft, whatever failed it; and
					// a canceled run must not complete with warnings over the
					// reads it never made.
					if s.Kind == KindEvents || s.Required || ctx.Err() != nil {
						return nil, wrapped
					}

					// An identity the query may not carry is refused the same
					// way however often the pass is rerun, which is what its
					// own warning code says: it names a resource to fix rather
					// than a store to wait for.
					notQueryable := errors.Is(err, errNotQueryable)

					// Only a failure of the store says anything about the next
					// draft. Counting a refusal, or an answer this one resource
					// shaped, would leave the metric out of the resources whose
					// own answers are fine.
					if !notQueryable && !errors.Is(err, ErrAnswerShape) {
						failures[s]++
					}
					code := WarningCounterSourceFailed
					if notQueryable {
						code = WarningCounterIdentityNotQueryable
					}
					warnings = append(warnings, warningOf(ru.Resource, d, s.Metric, code, wrapped.Error()))
					continue
				}
				delete(failures, s)

				// The write is in place, into the map metering built: every
				// copy of the draft shares it, so the caller's result carries
				// the counter without anything being handed back.
				d.Usage[s.Metric] = value
			}
		}
	}

	return warnings, nil
}

// measuring names what an error or a warning of the pass is about: the metric,
// the resource it is measured for, and the interval it is measured over.
func measuring(r source.Resource, d *metering.UsageDraft, metric string) string {
	return fmt.Sprintf("measuring %s of %s/%s/%s over [%s, %s)",
		metric, r.Cloud, r.ResourceType, r.ResourceID,
		d.FromTS.Format(time.RFC3339Nano), d.ToTS.Format(time.RFC3339Nano))
}

// warningOf names the metric one draft is billed without, and why.
func warningOf(r source.Resource, d *metering.UsageDraft, metric, code, detail string) Warning {
	return Warning{
		Cloud:        r.Cloud,
		ResourceType: r.ResourceType,
		ResourceID:   r.ResourceID,
		Metric:       metric,
		FromTS:       d.FromTS,
		ToTS:         d.ToTS,
		Code:         code,
		Detail:       detail,
	}
}

// measure reads one source's value for one draft: an events source counts the
// events of its type inside the draft's interval, a metricsql source queries the
// draft's own window at its end.
//
// Both are carried as the quantity every usage value is rendered at, four
// decimal places, whatever measured them. A count written as the bare integer it
// is would render as 5 where a query result renders as 5.0000, and a metric
// moved from one kind to the other would split its own history into two jsonb
// numbers under one name, in records no later run may rewrite.
func (m *Measurer) measure(
	ctx context.Context, r source.Resource, d *metering.UsageDraft, s Source,
) (money.Quantity, error) {
	if s.Kind == KindEvents {
		count, err := m.events.CountEvents(ctx, r, s.EventType, d.FromTS, d.ToTS)
		if err != nil {
			return money.Quantity{}, err
		}
		return money.NewQuantity(decimal.NewFromInt(count)), nil
	}

	expr, err := RenderQuery(s.Query, r.Cloud, r.ResourceID, d.ProjectID, d.Seconds)
	if err != nil {
		return money.Quantity{}, fmt.Errorf("%w: %w", errNotQueryable, err)
	}
	value, err := m.vm.Query(ctx, expr, d.ToTS)
	if err != nil {
		return money.Quantity{}, err
	}
	return money.NewQuantity(money.RoundQuantity(value)), nil
}
