package refdoc

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// integrationSuffix ends the key an Alertmanager receiver names a delivery
// integration with, webhook_configs and email_configs among them.
const integrationSuffix = "_configs"

// The keys of a child route that say what it matches and what hangs under it
// rather than what it changes about its parent.
const (
	matchersKey = "matchers"
	routesKey   = "routes"
)

// ruleFile is the shape vmalert reads its rules in: groups of rules, each group
// on a timer of its own.
type ruleFile struct {
	Groups []ruleGroup `yaml:"groups"`
}

// ruleGroup is one group and the interval it is evaluated on.
type ruleGroup struct {
	Name     string       `yaml:"name"`
	Interval string       `yaml:"interval"`
	Rules    []alertOrRec `yaml:"rules"`
}

// alertOrRec is either an alerting rule, which names an Alert, or a recording
// rule, which names a Record. Only one of the two is ever set.
type alertOrRec struct {
	Alert       string            `yaml:"alert"`
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// AlertRules renders the rules an evaluator loads: a paragraph per group and a
// section per rule, in the order the file writes them.
//
// The summary and the expression are fenced rather than written into the prose.
// Both carry Go templates such as {{ $labels.cloud }}, which the site would
// otherwise read as an interpolation of its own and evaluate against nothing.
func AlertRules(rules []byte) (string, error) {
	var file ruleFile
	if err := yaml.Unmarshal(rules, &file); err != nil {
		return "", fmt.Errorf("refdoc: rules: %w", err)
	}
	if len(file.Groups) == 0 {
		return "", errors.New("refdoc: no groups")
	}

	var b strings.Builder
	for _, group := range file.Groups {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(groupParagraph(group) + "\n")

		for i, rule := range group.Rules {
			name := rule.Alert
			if name == "" {
				name = rule.Record
			}
			if name == "" {
				return "", fmt.Errorf("refdoc: rule %d of group %s has no name", i, group.Name)
			}
			writeRule(&b, name, rule)
		}
	}
	return b.String(), nil
}

// groupParagraph names the group and how often it is evaluated.
func groupParagraph(group ruleGroup) string {
	if group.Interval == "" {
		return "Group " + code(group.Name) + "."
	}
	return "Group " + code(group.Name) + ", evaluated every " + code(group.Interval) + "."
}

// writeRule renders one rule under a heading of its own. A recording rule
// writes a series back rather than firing, so it carries neither a severity nor
// a summary and says what it is instead.
func writeRule(b *strings.Builder, name string, rule alertOrRec) {
	b.WriteString("\n### " + code(name) + "\n")

	if rule.Alert == "" {
		writeBlock(b, "Recorded series.\n")
	} else {
		writeBlock(b, table([]string{"Property", "Value"}, [][]string{
			{"Severity", codeOrNone(rule.Labels["severity"])},
			{"For", codeOrNone(rule.For)},
			{"Runbook", codeOrNone(rule.Annotations["runbook"])},
		}))
		writeBlock(b, "Summary:\n")
		writeBlock(b, Fenced("text", []byte(rule.Annotations["summary"])))
	}
	writeBlock(b, "Expression:\n")
	writeBlock(b, Fenced("promql", []byte(rule.Expr)))
}

// routingFile is the part of an Alertmanager configuration a page reads.
type routingFile struct {
	Route     *routeNode       `yaml:"route"`
	Receivers []map[string]any `yaml:"receivers"`
}

// routeNode is the root route: where an alert is delivered and how alerts are
// grouped on the way. A child route is read as a mapping, because what it
// changes about its parent is whichever keys it repeats.
type routeNode struct {
	Receiver       string           `yaml:"receiver"`
	GroupBy        []string         `yaml:"group_by"`
	GroupWait      string           `yaml:"group_wait"`
	GroupInterval  string           `yaml:"group_interval"`
	RepeatInterval string           `yaml:"repeat_interval"`
	Routes         []map[string]any `yaml:"routes"`
}

// AlertRouting renders where a fired alert goes: the grouping the root route
// holds every alert to, what each child route changes about it, and whether a
// receiver carries a delivery integration at all.
func AlertRouting(cfg []byte) (string, error) {
	var file routingFile
	if err := yaml.Unmarshal(cfg, &file); err != nil {
		return "", fmt.Errorf("refdoc: routing: %w", err)
	}
	if file.Route == nil {
		return "", errors.New("refdoc: no route")
	}

	var b strings.Builder
	b.WriteString(table([]string{"Setting", "Value"}, [][]string{
		{"Receiver", codeOrNone(file.Route.Receiver)},
		{"Group by", codeSpans(file.Route.GroupBy, ", ")},
		{"Group wait", codeOrNone(file.Route.GroupWait)},
		{"Group interval", codeOrNone(file.Route.GroupInterval)},
		{"Repeat interval", codeOrNone(file.Route.RepeatInterval)},
	}))
	writeBlock(&b, table([]string{"Matchers", "Overrides"}, childRouteRows(file.Route.Routes)))
	writeBlock(&b, receiverSentence(file.Receivers)+"\n")
	return b.String(), nil
}

// childRouteRows is one row per child route: what it matches, and what it
// changes about the grouping it inherits.
func childRouteRows(children []map[string]any) [][]string {
	rows := make([][]string, 0, len(children))
	for _, child := range children {
		rows = append(rows, []string{
			codeSpans(textList(child[matchersKey]), "; "),
			codeSpans(overrides(child), ", "),
		})
	}
	return rows
}

// overrides are the settings a child route states itself, in key order. What it
// matches and what hangs under it are not settings of its own.
func overrides(child map[string]any) []string {
	var settings []string
	for _, key := range slices.Sorted(maps.Keys(child)) {
		if key == matchersKey || key == routesKey {
			continue
		}
		settings = append(settings, fmt.Sprintf("%s: %v", key, child[key]))
	}
	return settings
}

// receiverSentence names the receivers and says whether an alert reaching one
// is delivered anywhere. A receiver naming no integration holds the alert in
// Alertmanager, where the UI and amtool show it and nothing else does.
func receiverSentence(receivers []map[string]any) string {
	if len(receivers) == 0 {
		return "The configuration names no receiver."
	}

	names := make([]string, 0, len(receivers))
	var wired []string
	for _, receiver := range receivers {
		name := code(textValue(receiver["name"]))
		names = append(names, name)
		if carriesIntegration(receiver) {
			wired = append(wired, name)
		}
	}

	sentence := "The receivers are " + strings.Join(names, ", ") + "; "
	switch len(wired) {
	case 0:
		return sentence + "none carries an integration."
	case 1:
		return sentence + wired[0] + " carries an integration."
	}
	return sentence + strings.Join(wired, ", ") + " carry an integration."
}

// carriesIntegration reports whether a receiver names somewhere to deliver to.
func carriesIntegration(receiver map[string]any) bool {
	for key := range receiver {
		if strings.HasSuffix(key, integrationSuffix) {
			return true
		}
	}
	return false
}

// textList reads a decoded YAML sequence as the strings it holds.
func textList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}

	texts := make([]string, 0, len(items))
	for _, item := range items {
		texts = append(texts, textValue(item))
	}
	return texts
}

// textValue reads a decoded YAML scalar as the text a cell shows.
func textValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
