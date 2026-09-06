package refdoc

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// tallyPrefix names every series this project owns. A registry also carries the
// Go runtime and the process collectors, which report the process rather than
// the product and belong on no page of this repository.
const tallyPrefix = "tally_"

// counterSuffix is what the exposition format asks the name of a counter to end
// in.
const counterSuffix = "_total"

// The two words a page names a series by.
const (
	wordCounter = "counter"
	wordGauge   = "gauge"
)

// quotedString matches a Go string literal, the escaped quotes inside it
// included, which is how a descriptor writes a name and a help text.
const quotedString = `"(?:[^"\\]|\\.)*"`

// descPattern reads a descriptor back. A descriptor is the only place a
// registry states the labels of a collector that carries no child yet, and its
// String method is the only way out of the type, so the text it writes is what
// this parses.
var descPattern = regexp.MustCompile(
	`^Desc\{fqName: (` + quotedString + `), help: (` + quotedString +
		`), unit: (?:` + quotedString + `), constLabels: \{.*\}, variableLabels: \{(.*)\}\}$`)

// constrainedLabel is how a descriptor writes a label whose values its
// collector bounds. A page names the label; the bound is the collector's
// business.
var constrainedLabel = regexp.MustCompile(`^c\((.*)\)$`)

// Metrics renders the series a registry carries: one row per tally_ series in
// name order, with the labels it is broken down by and what the code says it
// counts.
//
// The type comes from the exposition format where the registry holds a value,
// and from the name where it does not, because a vector without a child is
// gathered as nothing. A series whose type and name disagree is refused rather
// than rendered: the name is what a query is written against, so the two saying
// different things is a fault in the instrument.
func Metrics(reg *prometheus.Registry) (string, error) {
	gathered, err := gatheredTypes(reg)
	if err != nil {
		return "", err
	}

	var rows [][]string
	for _, descriptor := range describeAll(reg) {
		instrument, err := parseDescriptor(descriptor)
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(instrument.name, tallyPrefix) {
			continue
		}
		word, err := typeWord(instrument.name, gathered)
		if err != nil {
			return "", err
		}
		rows = append(rows, []string{
			code(instrument.name),
			word,
			labelCell(instrument.labels),
			escapePlaceholders(oneLine(instrument.help)),
		})
	}
	if len(rows) == 0 {
		return "", errors.New("refdoc: the registry carries no tally_ metric")
	}

	slices.SortFunc(rows, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	return table([]string{"Metric", "Type", "Labels", "Help"}, rows), nil
}

// instrument is one descriptor as a row reads it.
type instrument struct {
	name   string
	help   string
	labels []string
}

// describeAll collects every descriptor the registry holds. Describe writes
// into the channel while holding a read lock, so the channel is drained as the
// registry writes into it, and it is drained to the end even when a descriptor
// further down is one this package refuses.
func describeAll(reg *prometheus.Registry) []*prometheus.Desc {
	descriptions := make(chan *prometheus.Desc)
	go func() {
		reg.Describe(descriptions)
		close(descriptions)
	}()

	var descriptors []*prometheus.Desc
	for descriptor := range descriptions {
		descriptors = append(descriptors, descriptor)
	}
	return descriptors
}

// gatheredTypes is the type the exposition format reports for each series the
// registry currently holds a value for.
func gatheredTypes(reg *prometheus.Registry) (map[string]string, error) {
	families, err := reg.Gather()
	if err != nil {
		return nil, fmt.Errorf("refdoc: gathering: %w", err)
	}

	types := make(map[string]string, len(families))
	for _, family := range families {
		types[family.GetName()] = strings.ToLower(family.GetType().String())
	}
	return types, nil
}

// typeWord is the word a page names the series by.
func typeWord(name string, gathered map[string]string) (string, error) {
	named := wordGauge
	if strings.HasSuffix(name, counterSuffix) {
		named = wordCounter
	}
	registered, ok := gathered[name]
	if !ok {
		return named, nil
	}
	if registered != named {
		return "", fmt.Errorf("refdoc: %s is a %s but its name says %s", name, registered, named)
	}
	return registered, nil
}

// parseDescriptor reads the name, the help text and the labels out of what a
// descriptor prints.
func parseDescriptor(descriptor *prometheus.Desc) (instrument, error) {
	text := descriptor.String()
	match := descPattern.FindStringSubmatch(text)
	if match == nil {
		return instrument{}, fmt.Errorf("refdoc: cannot parse descriptor %q", text)
	}

	name, nameErr := strconv.Unquote(match[1])
	help, helpErr := strconv.Unquote(match[2])
	if nameErr != nil || helpErr != nil {
		return instrument{}, fmt.Errorf("refdoc: cannot parse descriptor %q", text)
	}
	return instrument{name: name, help: help, labels: labelNames(match[3])}, nil
}

// labelNames are the variable labels of a descriptor.
func labelNames(list string) []string {
	if list == "" {
		return nil
	}

	names := strings.Split(list, ",")
	for i, name := range names {
		if match := constrainedLabel.FindStringSubmatch(name); match != nil {
			names[i] = match[1]
		}
	}
	return names
}

// labelCell lists the labels a series is broken down by, or none for a series
// that carries one value.
func labelCell(labels []string) string {
	if len(labels) == 0 {
		return "none"
	}

	spans := make([]string, 0, len(labels))
	for _, label := range labels {
		spans = append(spans, code(label))
	}
	return strings.Join(spans, ", ")
}
