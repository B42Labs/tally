package refdoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// dashboardModel is the part of the Grafana model a page reads. Everything else
// the file holds is layout.
type dashboardModel struct {
	UID        string           `json:"uid"`
	Title      string           `json:"title"`
	Panels     []dashboardPanel `json:"panels"`
	Templating struct {
		List []dashboardVariable `json:"list"`
	} `json:"templating"`
}

// dashboardVariable is one variable a viewer picks a value for.
type dashboardVariable struct {
	Name  string `json:"name"`
	Multi bool   `json:"multi"`
	Query string `json:"query"`
}

// dashboardPanel carries a nested panel list of its own: a row panel holds the
// panels below it once the row is collapsed.
type dashboardPanel struct {
	Title   string            `json:"title"`
	Type    string            `json:"type"`
	Panels  []dashboardPanel  `json:"panels"`
	Targets []dashboardTarget `json:"targets"`
}

// dashboardTarget is one query a panel draws.
type dashboardTarget struct {
	Expr string `json:"expr"`
}

// Dashboards renders the provisioned dashboards: one section per file in name
// order, with the variables it takes and the query behind every panel.
//
// A text carrying a Go template, a backtick or a line break is refused rather
// than rendered. A table cell is not fenced, so the site would read the braces
// as an interpolation of its own, a backtick would close the code span the text
// stands in and let the rest of it out, and a row of a table ends at the first
// line break; a dashboard that needs any of them belongs in a fenced block a
// page writes by hand.
func Dashboards(files map[string][]byte) (string, error) {
	if len(files) == 0 {
		return "", errors.New("refdoc: no dashboards")
	}

	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(files)) {
		var model dashboardModel
		if err := json.Unmarshal(files[name], &model); err != nil {
			return "", fmt.Errorf("refdoc: %s: %w", name, err)
		}
		panels := flattenPanels(model.Panels)
		if err := checkCells(name, model, panels); err != nil {
			return "", err
		}

		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### " + code(name) + "\n")
		writeBlock(&b, "Title "+code(model.Title)+", uid "+code(model.UID)+".\n")
		writeBlock(&b, table([]string{"Variable", "Multi", "Query"}, variableRows(model)))
		writeBlock(&b, table([]string{"Panel", "Type", "Expression"}, panelRows(panels)))
	}
	return b.String(), nil
}

// variableRows is one row per variable the dashboard declares.
func variableRows(model dashboardModel) [][]string {
	rows := make([][]string, 0, len(model.Templating.List))
	for _, variable := range model.Templating.List {
		rows = append(rows, []string{
			code(variable.Name),
			yesNo(variable.Multi),
			codeOrNone(variable.Query),
		})
	}
	return rows
}

// flattenPanels returns every panel of a dashboard, the row itself and the
// panels it nests included.
func flattenPanels(panels []dashboardPanel) []dashboardPanel {
	all := make([]dashboardPanel, 0, len(panels))
	for _, panel := range panels {
		all = append(all, panel)
		all = append(all, flattenPanels(panel.Panels)...)
	}
	return all
}

// panelRows is one row per query a panel draws, and one row saying so for a
// panel that draws none.
func panelRows(panels []dashboardPanel) [][]string {
	var rows [][]string
	for _, panel := range panels {
		drawn := 0
		for _, target := range panel.Targets {
			if target.Expr == "" {
				continue
			}
			drawn++
			rows = append(rows, []string{
				escapePlaceholders(panel.Title),
				code(panel.Type),
				code(target.Expr),
			})
		}
		if drawn == 0 {
			rows = append(rows, []string{escapePlaceholders(panel.Title), code(panel.Type), "none"})
		}
	}
	return rows
}

// checkCells refuses a dashboard carrying a text a section cannot state. Every
// string a section renders stands in a table cell or in the sentence above the
// tables, and neither is fenced, so one text getting out takes the rest of the
// section with it.
func checkCells(file string, model dashboardModel, panels []dashboardPanel) error {
	texts := []string{model.Title, model.UID}
	for _, variable := range model.Templating.List {
		texts = append(texts, variable.Name, variable.Query)
	}
	for _, panel := range panels {
		texts = append(texts, panel.Title, panel.Type)
		for _, target := range panel.Targets {
			texts = append(texts, target.Expr)
		}
	}

	for _, text := range texts {
		if breaksCell(text) {
			return fmt.Errorf("refdoc: %s: %q holds a backtick, a mustache or a "+
				"line break, render it fenced", file, text)
		}
	}
	return nil
}

// breaksCell reports whether text would leave the cell it stands in: a backtick
// closes the code span around it, the site reads a mustache as an interpolation
// of its own, and a row of a table ends at the first line break, which drops
// the rest of the text and every row below it out of the table. Grafana writes
// a query over several lines as a matter of routine.
func breaksCell(text string) bool {
	return strings.ContainsAny(text, "`\n\r") ||
		strings.Contains(text, "{{") || strings.Contains(text, "}}")
}
