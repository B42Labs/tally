package httpui

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/adjustments"
	"github.com/b42labs/tally/internal/engine/attribution"
	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/statements"
)

// statsView is what the run page says about the stats a run stored: whether it
// stored any, whether the console can read them, what they count, and every
// list of findings, one table each. A table is named after the member of the
// stats it lists, so a reader of an export's run.json finds each list under the
// same name.
type statsView struct {
	// Stored is false for a run that stored no stats, which is a run still
	// running: its row holds the column's empty default.
	Stored bool
	// Unreadable is why the console cannot read the stats, and Raw is what was
	// stored, shown in their place.
	Unreadable string
	Raw        string
	// Correction says the counts are a correction's, which calls its
	// statements credit notes and counts the deltas it wrote.
	Correction bool
	Values     runs.CorrectionStats
	// Findings is how many rows the lists hold together, so a run that found
	// nothing says so.
	Findings int

	Warnings             listing[runs.Warning]
	MeteringWarnings     listing[meteringRow]
	CounterWarnings      listing[counterRow]
	AttributionWarnings  listing[attributionRow]
	AdjustmentWarnings   listing[adjustments.Warning]
	Unpriced             listing[rating.UnpricedResourceType]
	UnreadableQuantities listing[rating.UnreadableQuantity]
	UnregisteredProjects listing[unregisteredRow]
	Violations           listing[violationRow]
}

// meteringRow is one warning of the metering pass with the page of the
// resource it is about.
type meteringRow struct {
	metering.Warning
	Link string
}

// counterRow is one warning of the counter pass with the page of the resource
// it is about.
type counterRow struct {
	counters.Warning
	Link string
}

// attributionRow is one warning of the attribution pass with the page of the
// project it is about. Relation is the losing relation, which a cycle warning
// does not have.
type attributionRow struct {
	attribution.Warning
	Relation string
	Link     string
}

// unregisteredRow is one project the run met resources of and no registry row
// names. It has no project page, so it leads to the resources it holds.
type unregisteredRow struct {
	statements.UnregisteredProject
	Link string
}

// violationRow is one invariant violation of one resource. The engine groups
// violations by resource; the table holds one row per violation.
type violationRow struct {
	Cloud        string
	ResourceType string
	ResourceID   string
	Invariant    string
	Detail       string
	Link         string
}

// The columns of the stats tables, one set per list.
var (
	runWarningColumns = []column[runs.Warning]{
		textCol("code", func(r runs.Warning) string { return r.Code }),
		textCol("detail", func(r runs.Warning) string { return r.Detail }),
	}
	meteringWarningColumns = []column[meteringRow]{
		textCol("code", func(r meteringRow) string { return r.Code }),
		textCol("cloud", func(r meteringRow) string { return r.Cloud }),
		textCol("resource type", func(r meteringRow) string { return r.ResourceType }),
		textCol("resource", func(r meteringRow) string { return r.ResourceID }),
	}
	counterWarningColumns = []column[counterRow]{
		textCol("code", func(r counterRow) string { return r.Code }),
		textCol("cloud", func(r counterRow) string { return r.Cloud }),
		textCol("resource type", func(r counterRow) string { return r.ResourceType }),
		textCol("resource", func(r counterRow) string { return r.ResourceID }),
		textCol("metric", func(r counterRow) string { return r.Metric }),
		textCol("from", func(r counterRow) string { return stamp(r.FromTS) }),
		textCol("to", func(r counterRow) string { return stamp(r.ToTS) }),
		textCol("detail", func(r counterRow) string { return r.Detail }),
	}
	attributionWarningColumns = []column[attributionRow]{
		textCol("code", func(r attributionRow) string { return r.Code }),
		textCol("project", func(r attributionRow) string { return r.ProjectID.String() }),
		textCol("relation", func(r attributionRow) string { return r.Relation }),
	}
	adjustmentWarningColumns = []column[adjustments.Warning]{
		textCol("code", func(r adjustments.Warning) string { return r.Code }),
		textCol("relation", func(r adjustments.Warning) string { return r.RelationID }),
		textCol("target platform", func(r adjustments.Warning) string { return r.TargetPlatform }),
		textCol("target", func(r adjustments.Warning) string { return r.TargetID }),
	}
	unpricedColumns = []column[rating.UnpricedResourceType]{
		textCol("platform", func(r rating.UnpricedResourceType) string { return r.Platform }),
		textCol("resource type", func(r rating.UnpricedResourceType) string { return r.ResourceType }),
		countCol("count", func(r rating.UnpricedResourceType) int64 { return int64(r.Count) }),
	}
	unreadableColumns = []column[rating.UnreadableQuantity]{
		textCol("platform", func(r rating.UnreadableQuantity) string { return r.Platform }),
		textCol("resource type", func(r rating.UnreadableQuantity) string { return r.ResourceType }),
		textCol("field", func(r rating.UnreadableQuantity) string { return r.Field }),
		countCol("count", func(r rating.UnreadableQuantity) int64 { return int64(r.Count) }),
	}
	unregisteredColumns = []column[unregisteredRow]{
		textCol("cloud", func(r unregisteredRow) string { return r.Cloud }),
		textCol("project", func(r unregisteredRow) string { return r.ProjectID }),
		countCol("resources", func(r unregisteredRow) int64 { return int64(r.Resources) }),
	}
	violationColumns = []column[violationRow]{
		textCol("cloud", func(r violationRow) string { return r.Cloud }),
		textCol("resource type", func(r violationRow) string { return r.ResourceType }),
		textCol("resource", func(r violationRow) string { return r.ResourceID }),
		textCol("invariant", func(r violationRow) string { return r.Invariant }),
		textCol("detail", func(r violationRow) string { return r.Detail }),
	}
)

// readRunStats reads the stats a run stored. The bool is false for bytes that
// hold nothing, which is the column before the run wrote to it: no bytes, JSON
// null, or an object without members. The decoding is strict: a member the
// engine's types do not have is refused rather than dropped, because a page
// that silently left out part of what a run reported would read as a run that
// reported less.
func readRunStats(raw []byte) (runs.CorrectionStats, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return runs.CorrectionStats{}, false, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return runs.CorrectionStats{}, true, err
	}
	if len(members) == 0 {
		return runs.CorrectionStats{}, false, nil
	}

	var stats runs.CorrectionStats
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stats); err != nil {
		return runs.CorrectionStats{}, true, err
	}
	return stats, true, nil
}

// buildRunStats lays the stats of a run out. Stats the console cannot read are
// shown as they were stored, beside the reason, and leave the rest of the page
// standing: the page is about the run, and its statements and deltas read fine
// without them. They are reported on the page and not logged, the way the
// project page reports relation adjustments it cannot read.
func buildRunStats(r *http.Request, run store.Run) statsView {
	stats, stored, err := readRunStats(run.Stats)
	view := statsView{Stored: stored, Correction: storesCreditNotes(run)}
	if err != nil {
		view.Unreadable = err.Error()
		view.Raw = rawStats(run.Stats)
		return view
	}
	if !stored {
		return view
	}

	metered := make([]meteringRow, 0, len(stats.MeteringWarnings))
	for _, warning := range stats.MeteringWarnings {
		metered = append(metered, meteringRow{
			Warning: warning,
			Link:    link("/resource", "cloud", warning.Cloud, "type", warning.ResourceType, "id", warning.ResourceID),
		})
	}
	counted := make([]counterRow, 0, len(stats.CounterWarnings))
	for _, warning := range stats.CounterWarnings {
		counted = append(counted, counterRow{
			Warning: warning,
			Link:    link("/resource", "cloud", warning.Cloud, "type", warning.ResourceType, "id", warning.ResourceID),
		})
	}
	attributed := make([]attributionRow, 0, len(stats.AttributionWarnings))
	for _, warning := range stats.AttributionWarnings {
		relation := warning.RelationID
		if relation == "" {
			relation = absent
		}
		attributed = append(attributed, attributionRow{
			Warning:  warning,
			Relation: relation,
			Link:     link("/project", "id", warning.ProjectID.String()),
		})
	}
	unregistered := make([]unregisteredRow, 0, len(stats.UnregisteredProjects))
	for _, project := range stats.UnregisteredProjects {
		unregistered = append(unregistered, unregisteredRow{
			UnregisteredProject: project,
			Link: link("/resources", "cloud", project.Cloud, "project_id", project.ProjectID,
				statusParameter, "all"),
		})
	}
	var violations []violationRow
	for _, resource := range stats.Violations {
		for _, violation := range resource.Violations {
			violations = append(violations, violationRow{
				Cloud:        resource.Cloud,
				ResourceType: resource.ResourceType,
				ResourceID:   resource.ResourceID,
				Invariant:    violation.Invariant,
				Detail:       violation.Detail,
				Link: link("/resource", "cloud", resource.Cloud, "type", resource.ResourceType,
					"id", resource.ResourceID),
			})
		}
	}

	view.Values = stats
	view.Warnings = tabulate(r, "warnings", runWarningColumns, stats.Warnings)
	view.MeteringWarnings = tabulate(r, "metering_warnings", meteringWarningColumns, metered)
	view.CounterWarnings = tabulate(r, "counter_warnings", counterWarningColumns, counted)
	view.AttributionWarnings = tabulate(r, "attribution_warnings", attributionWarningColumns, attributed)
	view.AdjustmentWarnings = tabulate(r, "adjustment_warnings", adjustmentWarningColumns, stats.AdjustmentWarnings)
	view.Unpriced = tabulate(r, "unpriced", unpricedColumns, stats.Unpriced)
	view.UnreadableQuantities = tabulate(r, "unreadable", unreadableColumns, stats.Unreadable)
	view.UnregisteredProjects = tabulate(r, "unregistered_projects", unregisteredColumns, unregistered)
	view.Violations = tabulate(r, "violations", violationColumns, violations)
	view.Findings = len(stats.Warnings) + len(metered) + len(counted) + len(attributed) +
		len(stats.AdjustmentWarnings) + len(stats.Unpriced) + len(stats.Unreadable) + len(unregistered) +
		len(violations)
	return view
}

// rawStats is the stored stats as the page shows them when it cannot read
// them: indented when they are JSON at all, and as the bytes they are when not.
func rawStats(raw []byte) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	return pretty(value)
}
