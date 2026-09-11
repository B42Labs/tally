package httpui

import (
	"encoding/json"
	"html/template"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
)

// absent is what a template prints where a value is not there: a timestamp
// that was never written, a payload nothing stored, a modifier map a catalog
// entry left out. unknown is what it prints where a value exists but the
// console cannot know it: the creation and the lifetime of a resource whose
// history starts without a create when the API served no first event for it
// either, and on a resource page the creation of such a history.
const (
	absent  = "none"
	unknown = "unknown"
)

// funcMap is what every page is parsed with. A number reaches a page as a
// decimal and becomes text here, at the scale its kind is rendered at, so no
// page decides on its own how many places an amount carries.
func funcMap() template.FuncMap {
	return template.FuncMap{
		"amount":        amount,
		"quantity":      quantity,
		"rate":          rate,
		"price":         price,
		"stamp":         stamp,
		"optStamp":      optStamp,
		"zeroStamp":     zeroStamp,
		"idText":        idText,
		"optString":     optString,
		"pretty":        pretty,
		"stateClass":    stateClass,
		"emptyText":     emptyText,
		"statementLink": statementLink,
		"zeroClass":     zeroClass,
	}
}

// statementLink is the page of one stored statement, addressed the way the
// engine stores it: the run that wrote it and the key it was written under.
func statementLink(runID uuid.UUID, key string) string {
	return link("/statement", "run", runID.String(), "key", key)
}

// zeroClass is the class a cell of exactly zero is muted with, appended to the
// cell's other classes, and nothing for any other value. A bill of a simulated
// month is mostly zeros, and the few real amounts have to stand out of them.
func zeroClass(d decimal.Decimal) string {
	if d.IsZero() {
		return " zero"
	}
	return ""
}

// emptyText is what a table without rows says. A table the filter emptied says
// so; a table that had nothing to filter says what the page says about it.
func emptyText(view tableView, otherwise string) string {
	if view.Query != "" && view.Total > 0 {
		return "no row matches the filter"
	}
	return otherwise
}

// amount renders money at the two places every monetary value is rounded to.
func amount(d decimal.Decimal) string {
	return d.StringFixed(money.AmountPlaces)
}

// quantity renders a usage quantity at the four places usage is rounded to.
func quantity(d decimal.Decimal) string {
	return d.StringFixed(money.QuantityPlaces)
}

// rate renders the rate of a pricing adjustment at the six places the
// adjustments schema admits.
func rate(d decimal.Decimal) string {
	return d.StringFixed(money.RatePlaces)
}

// price renders a catalog price with the digits it was imported with. A price
// of 0.00005 is five hundredths of a millicent per unit hour, and rounding it
// to a money scale would print it as nothing at all.
func price(d decimal.Decimal) string {
	return d.String()
}

// stamp renders an instant in UTC, so two rows of one table are comparable
// whatever zone their values arrived in.
func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// optStamp renders an instant the API leaves null, a deletion that has not
// happened for example.
func optStamp(t *time.Time) string {
	if t == nil {
		return absent
	}
	return stamp(*t)
}

// zeroStamp renders an instant the store reads a NULL column as, the completion
// of a run that did not finish for example.
func zeroStamp(t time.Time) string {
	if t.IsZero() {
		return absent
	}
	return stamp(t)
}

// optString renders a string the API leaves null, falling back to what stands
// in for it: a project without a name is shown under its external id.
func optString(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

// pretty renders a stored JSON value as indented text. A value that is not
// there and one that is empty both read as absent, because an empty object on a
// page says less than the word for it.
func pretty(v any) string {
	switch value := v.(type) {
	case nil:
		return absent
	case *map[string]interface{}:
		if value == nil || len(*value) == 0 {
			return absent
		}
		v = *value
	case map[string]interface{}:
		if len(value) == 0 {
			return absent
		}
	}

	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// Everything on this path was decoded from JSON a moment ago, so a
		// failure here is a value no page can show. Saying so beats an empty
		// cell that looks like a value nobody stored.
		return "unreadable: " + err.Error()
	}
	return string(out)
}

// stateClass maps a resource state to the CSS class its colour is defined
// under. A state is free text the collectors write, so everything outside the
// class alphabet becomes a hyphen and a state nothing is defined for falls back
// to the lane's own fill.
func stateClass(state string) string {
	var b strings.Builder
	b.WriteString("state-")
	for _, r := range strings.ToLower(state) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
