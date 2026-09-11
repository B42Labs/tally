package httpui

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// The resources page lists the part of the fleet the Reporting API serves
// under one status, and of those rows the ones the chosen time keeps. The
// status is the API's own filter, active by default. The time is the
// console's, applied to the rows the API served.
//
// A viewer reads the fleet one of two ways, and never both at once: at an
// instant, which answers what runs right now or what ran at one moment, or
// over a window, which answers what lived between two moments. Which of the
// two a request means is the mode parameter, and a request that names none
// means the window when it carries a bound of one and the instant otherwise.
// The parameters of the other way are ignored rather than combined, so one
// page is one question.
//
// The instant is at, now when it is absent: a resource existed at it when it
// was created at or before it and not deleted at or before it. The window is
// from and to, a half-open span: a resource existed in it when it was created
// before to and not deleted at or before from. A window with neither bound is
// every row the page holds, whenever it lived. Every rule here reads a
// resource whose history starts without a create from its first event, the
// instant the fold starts billing it at.
//
// A deleted resource is only on the page when the status admits it, so
// looking back starts with switching the status to all. Every row carries the
// resource's lifetime in hours, from its creation to its deletion or to now,
// and folds its last payload.
const (
	// resourcePageLimit is how many rows the page asks the API for, the most
	// the API serves in one page. The filters run on the rows the page holds,
	// so a page that holds the whole fleet filters the whole fleet, and the
	// fleet of the simulated month fits.
	resourcePageLimit = 1000
	// secondsPerHour turns a lifetime in seconds into the hours a bill
	// counts in.
	secondsPerHour = 3600
	// atLayout is how the datetime-local inputs write and read an instant:
	// to the minute, without a zone, and read as UTC here.
	atLayout = "2006-01-02T15:04"
	// The query parameters of the fleet controls.
	statusParameter = "status"
	modeParameter   = "mode"
	fromParameter   = "from"
	toParameter     = "to"
	atParameter     = "at"
	// The two ways the fleet is read. The instant is the default and travels
	// as no parameter at all; the window is named, because a window with
	// neither bound is otherwise indistinguishable from the instant.
	modeInstant = "instant"
	modeWindow  = "window"
	// resourceTable is the name the resource listing's sort and filter
	// parameters carry.
	resourceTable = "resources"
)

// statuses are the parts of the fleet the API serves, in the order the switch
// prints them. The first is the API's default and the page's.
var statuses = []string{"active", "deleted", "all"}

// fleetState is what the request says about the fleet: which part of it, and
// which time it is read at. Window says which of the two ways was asked for,
// and the members of the other way are left at their zero values. A nil bound
// is a window open on that side, and Pinned is false while no instant was
// named, which is now.
type fleetState struct {
	Status string
	Window bool
	From   *time.Time
	To     *time.Time
	At     time.Time
	Pinned bool
}

// readFleet reads the status and the time the fleet is read at, each falling
// back to its default when absent. Only the parameters of the mode in effect
// are read: what the other way would have been asked with says nothing about
// this page and is not held against the request either. A status the API does
// not serve, a mode that is neither way, a bound or an instant that does not
// parse, and a window that ends before it starts are refused the way any
// unreadable parameter is.
func readFleet(r *http.Request, now time.Time) (fleetState, error) {
	query := r.URL.Query()
	state := fleetState{Status: statuses[0], At: now}

	if status := query.Get(statusParameter); status != "" {
		if !slices.Contains(statuses, status) {
			return fleetState{}, &paramError{name: statusParameter, reason: "is not active, deleted or all"}
		}
		state.Status = status
	}

	switch mode := query.Get(modeParameter); mode {
	case "":
		// A link written before the modes, or by hand, names a window by
		// carrying a bound of one.
		state.Window = query.Get(fromParameter) != "" || query.Get(toParameter) != ""
	case modeWindow:
		state.Window = true
	case modeInstant:
	default:
		return fleetState{}, &paramError{name: modeParameter, reason: "is not instant or window"}
	}

	if !state.Window {
		if at := query.Get(atParameter); at != "" {
			parsed, err := parseInstant(at)
			if err != nil {
				return fleetState{}, unreadableInstant(atParameter, err)
			}
			state.At, state.Pinned = parsed, true
		}
		return state, nil
	}

	var err error
	if state.From, err = optionalInstant(query, fromParameter); err != nil {
		return fleetState{}, err
	}
	if state.To, err = optionalInstant(query, toParameter); err != nil {
		return fleetState{}, err
	}
	if state.From != nil && state.To != nil && !state.To.After(*state.From) {
		return fleetState{}, &paramError{name: toParameter, reason: "is not after from"}
	}
	return state, nil
}

// optionalInstant reads a bound of the window, nil when it is not there.
func optionalInstant(query url.Values, name string) (*time.Time, error) {
	text := query.Get(name)
	if text == "" {
		return nil, nil
	}
	parsed, err := parseInstant(text)
	if err != nil {
		return nil, unreadableInstant(name, err)
	}
	return &parsed, nil
}

// parseInstant reads an instant as RFC 3339, the form a preset link carries,
// or as the form the datetime-local inputs write, which carries no zone and is
// read as UTC because every instant the console prints is UTC.
func parseInstant(text string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t.UTC(), nil
	}
	return time.ParseInLocation(atLayout, text, time.UTC)
}

// keep reports whether a row is shown: existing at the instant the page is
// read at, or having lived inside the window it is read over.
func keep(resource httpapi.Resource, state fleetState) bool {
	if !state.Window {
		return existedAt(resource, state.At)
	}
	return livedBetween(resource, state.From, state.To)
}

// lifetimeStart is the instant a resource's lifetime is counted from: its
// creation, or for a history that starts without a create its first event,
// which is where the fold starts the intervals a run bills. ok is false for a
// row that carries neither, which is what a Reporting API older than
// first_event_at serves.
func lifetimeStart(resource httpapi.Resource) (start time.Time, ok bool) {
	if resource.CreatedAt != nil {
		return *resource.CreatedAt, true
	}
	if resource.FirstEventAt.IsZero() {
		return time.Time{}, false
	}
	return resource.FirstEventAt, true
}

// firstEventMark follows the instant a created cell prints for a history that
// starts without a create, so the cell says which instant it is.
const firstEventMark = ", first event"

// createdText is what a resource's created cell prints: its creation, the
// instant of its first event marked as such, or unknown for a row carrying
// neither.
func createdText(resource httpapi.Resource) string {
	if resource.CreatedAt != nil {
		return stamp(*resource.CreatedAt)
	}
	if resource.FirstEventAt.IsZero() {
		return unknown
	}
	return stamp(resource.FirstEventAt) + firstEventMark
}

// livedBetween reports whether a resource lived inside a half-open window: it
// was created, or had its first event, before to and was not deleted at or
// before from. A nil bound is a window open on that side, and a window with
// neither bound holds every resource, whenever it lived.
func livedBetween(resource httpapi.Resource, from, to *time.Time) bool {
	if from != nil && resource.DeletedAt != nil && !resource.DeletedAt.After(*from) {
		return false
	}
	if to != nil {
		if start, ok := lifetimeStart(resource); ok && !start.Before(*to) {
			return false
		}
	}
	return true
}

// existedAt reports whether a resource existed at an instant: created at or
// before it, and not deleted at or before it. A resource whose history shows
// no create is read from its first event instead, and only a row carrying
// neither is taken to have existed all along.
func existedAt(resource httpapi.Resource, at time.Time) bool {
	if start, ok := lifetimeStart(resource); ok && start.After(at) {
		return false
	}
	return resource.DeletedAt == nil || resource.DeletedAt.After(at)
}

// lifetimeHours is how long a resource has lived, from its creation or, for a
// history that shows no create, its first event, to its deletion or to now for
// one still there, in hours at the two places the bills carry hours at. A row
// carrying neither reports that its lifetime is unknown.
func lifetimeHours(resource httpapi.Resource, now time.Time) (decimal.Decimal, bool) {
	start, ok := lifetimeStart(resource)
	if !ok {
		return decimal.Zero, false
	}
	return hoursBetween(start, lifetimeEnd(resource, now)), true
}

// lifetimeEnd is where a lifetime stops: the deletion, or now for a resource
// still there.
func lifetimeEnd(resource httpapi.Resource, now time.Time) time.Time {
	if resource.DeletedAt != nil {
		return *resource.DeletedAt
	}
	return now
}

// hoursBetween is the span of two instants in hours at two places, and zero
// for an end before its start.
func hoursBetween(start, end time.Time) decimal.Decimal {
	seconds := int64(end.Sub(start) / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	return money.Round2(money.Div(decimal.NewFromInt(seconds), decimal.NewFromInt(secondsPerHour)))
}

// lifecycleTimes is what a resource page prints for the creation and the
// lifetime of one resource. A history that starts without a create has no
// creation time; the page then names the event the history starts with and
// counts the lifetime from it, because that is where the fold starts the
// intervals a run bills.
func lifecycleTimes(lifecycle httpapi.Lifecycle, now time.Time) (created, lifetime string) {
	resource := lifecycle.Resource
	end := lifetimeEnd(resource, now)
	if resource.CreatedAt != nil {
		return stamp(*resource.CreatedAt), amount(hoursBetween(*resource.CreatedAt, end)) + " hours"
	}

	first, ok := firstEvent(lifecycle.Events)
	if !ok {
		return unknown, unknown
	}
	return fmt.Sprintf("%s, the history starts with %s at %s", unknown, first.EventType, stamp(first.Timestamp)),
		amount(hoursBetween(first.Timestamp, end)) + " hours since the first event"
}

// firstEvent is the earliest event of a history. The API serves a history in
// order, but the page reads the timestamps rather than trusting the order.
func firstEvent(events []httpapi.StoredEvent) (httpapi.StoredEvent, bool) {
	if len(events) == 0 {
		return httpapi.StoredEvent{}, false
	}
	first := events[0]
	for _, event := range events[1:] {
		if event.Timestamp.Before(first.Timestamp) {
			first = event
		}
	}
	return first, true
}

// firstEventText is the line the resources page adds for the rows whose
// history starts without a create and that show their first event instead,
// and nothing when there is none.
func firstEventText(rows int) string {
	switch rows {
	case 0:
		return ""
	case 1:
		return "1 row has no creation, because its history starts without a create: " +
			"it shows its first event instead, and its lifetime runs from that event"
	default:
		return fmt.Sprintf(
			"%d rows have no creation, because their histories start without a create: "+
				"they show their first event instead, and their lifetime runs from that event", rows)
	}
}

// unknownText is the line the resources page adds for the rows whose history
// starts without a create and that the API served no first event for either,
// and nothing when there is none.
func unknownText(rows int) string {
	switch rows {
	case 0:
		return ""
	case 1:
		return "1 row has no creation and no lifetime, because its history starts without a create " +
			"and the API served no first event for it"
	default:
		return fmt.Sprintf(
			"%d rows have no creation and no lifetime, because their histories start without a create "+
				"and the API served no first event for them", rows)
	}
}

// payloadSummary is what the folded payload of a row says: how many keys the
// payload carries, or nothing for a payload that is not there or empty, which
// the row prints as absent instead.
func payloadSummary(payload *map[string]interface{}) string {
	if payload == nil || len(*payload) == 0 {
		return ""
	}
	if len(*payload) == 1 {
		return "1 key"
	}
	return fmt.Sprintf("%d keys", len(*payload))
}

// choice is one option of a control: what it says, where it leads, and
// whether it is the one in effect.
type choice struct {
	Label   string
	Link    string
	Current bool
}

// fleetView is what the template renders above the resource table: the status
// switch, the switch between the two ways of reading the fleet, the inputs of
// the way in effect with the hidden fields their form carries, that way's
// presets, and what the filters left of the page.
type fleetView struct {
	Path   string
	Hidden []field
	Switch []choice
	Modes  []choice
	// Windowed says which inputs the form draws, the two bounds of a window
	// or the one instant, so that a page never asks for both.
	Windowed bool
	From     string
	To       string
	At       string
	Presets  []choice
	Count    string
	// Empty is what the table says when the filters left nothing of a page
	// that held rows; a page the API served empty says what it always said.
	Empty string
	// FirstEvent counts the rows kept whose history starts without a create
	// and that show their first event instead, and is empty when there is none.
	FirstEvent string
	// Unknown counts the rows kept whose history starts without a create and
	// that the API served no first event for, and is empty when there is none.
	Unknown string
}

// buildFleetView lays the controls out for one request. Every link the page
// draws is built over the parameters of the mode in effect alone, so that
// following one never leaves a page asking two questions at once; the switch
// to the other mode is what drops the one and names the other. The status
// links drop the cursor, because a cursor positions a walk through one status
// and means nothing in another, while the time links keep it, because they
// filter the page the cursor named. The instant input is left empty while
// nothing is pinned, so that the page opens on now without claiming an
// instant was chosen.
func buildFleetView(
	r *http.Request, state fleetState, now time.Time, kept, total, fromFirstEvent, unknown int, paged bool,
) fleetView {
	values := r.URL.Query()
	view := fleetView{
		Path:       r.URL.Path,
		Windowed:   state.Window,
		From:       inputValue(state.From),
		To:         inputValue(state.To),
		FirstEvent: firstEventText(fromFirstEvent),
		Unknown:    unknownText(unknown),
	}
	if state.Pinned {
		view.At = state.At.UTC().Format(atLayout)
	}

	// The two modes as their own links, each dropping what the other one is
	// asked with. The one in effect is what every other link on the page is
	// built over.
	asInstant := maps.Clone(values)
	asInstant.Del(fromParameter)
	asInstant.Del(toParameter)
	asInstant.Del(modeParameter)
	asWindow := maps.Clone(values)
	asWindow.Del(atParameter)
	asWindow.Set(modeParameter, modeWindow)
	view.Modes = []choice{
		{Label: "at an instant", Link: href(r.URL.Path, asInstant), Current: !state.Window},
		{Label: "over a window", Link: href(r.URL.Path, asWindow), Current: state.Window},
	}

	base := asInstant
	if state.Window {
		base = asWindow
	}

	view.Hidden = hiddenFields(values, modeParameter, fromParameter, toParameter, atParameter)

	for _, status := range statuses {
		linked := maps.Clone(base)
		linked.Del("cursor")
		linked.Set(statusParameter, status)
		if status == statuses[0] {
			linked.Del(statusParameter)
		}
		view.Switch = append(view.Switch, choice{
			Label: status, Link: href(r.URL.Path, linked), Current: status == state.Status,
		})
	}

	if state.Window {
		view.Presets = windowPresets(r.URL.Path, base, state, now)
	} else {
		view.Presets = instantPresets(r.URL.Path, base, state, now)
	}
	view.Count = fleetCount(state, kept, total, paged)
	if total > 0 && kept == 0 {
		view.Empty = "no resource of this page " + existence(state, 1)
	}
	return view
}

// hiddenFields is every parameter of a page as a form carries it through,
// except the ones the form asks for itself. A form that submits its page keeps
// the page's identifiers, its cursor, and the order and filter of every table
// on it that way.
func hiddenFields(values url.Values, own ...string) []field {
	var list []field
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if slices.Contains(own, key) {
			continue
		}
		for _, value := range values[key] {
			if value != "" {
				list = append(list, field{Name: key, Value: value})
			}
		}
	}
	return list
}

// inputValue is what a datetime-local input shows for a bound: the bound, or
// nothing for a side the window is open on.
func inputValue(bound *time.Time) string {
	if bound == nil {
		return ""
	}
	return bound.UTC().Format(atLayout)
}

// spanPresets are the four windows one click away, the spans behind now that a
// demo month is read over. Both bounds travel in every link, so a preset says
// the whole window rather than half of one.
func spanPresets(path string, values url.Values, from, to *time.Time, now time.Time) []choice {
	monthStart := firstOfThisMonthUTC(now)
	spans := []struct {
		label    string
		from, to time.Time
	}{
		{"last 24 hours", now.Add(-24 * time.Hour), now},
		{"last 7 days", now.Add(-7 * 24 * time.Hour), now},
		{"this month", monthStart, monthStart.AddDate(0, 1, 0)},
		{"last month", monthStart.AddDate(0, -1, 0), monthStart},
	}

	list := make([]choice, 0, len(spans))
	for _, span := range spans {
		linked := maps.Clone(values)
		linked.Set(fromParameter, stamp(span.from))
		linked.Set(toParameter, stamp(span.to))
		list = append(list, choice{
			Label:   span.label,
			Link:    href(path, linked),
			Current: from != nil && to != nil && from.Equal(span.from) && to.Equal(span.to),
		})
	}
	return list
}

// windowPresets are the spans the fleet is read over and any time, which is
// the window with neither bound and holds every row the page carries.
func windowPresets(path string, values url.Values, state fleetState, now time.Time) []choice {
	linked := maps.Clone(values)
	linked.Del(fromParameter)
	linked.Del(toParameter)
	return append(spanPresets(path, values, state.From, state.To, now), choice{
		Label:   "any time",
		Link:    href(path, linked),
		Current: state.From == nil && state.To == nil,
	})
}

// instantPresets are the instants one click away: now, which is no parameter
// at all, and four points behind it that a demo month is read at.
func instantPresets(path string, values url.Values, state fleetState, now time.Time) []choice {
	monthStart := firstOfThisMonthUTC(now)
	points := []struct {
		label string
		at    time.Time
	}{
		{"24 hours ago", now.Add(-24 * time.Hour)},
		{"7 days ago", now.Add(-7 * 24 * time.Hour)},
		{"start of this month", monthStart},
		{"start of last month", monthStart.AddDate(0, -1, 0)},
	}

	unpinned := maps.Clone(values)
	unpinned.Del(atParameter)
	list := []choice{{Label: "now", Link: href(path, unpinned), Current: !state.Pinned}}
	for _, point := range points {
		linked := maps.Clone(values)
		linked.Set(atParameter, stamp(point.at))
		list = append(list, choice{
			Label:   point.label,
			Link:    href(path, linked),
			Current: state.Pinned && state.At.Equal(point.at),
		})
	}
	return list
}

// fleetCount says what the filters left of the page: how many of the rows the
// API served existed at the instant or in the window.
func fleetCount(state fleetState, kept, total int, paged bool) string {
	scope := ""
	if paged {
		scope = " on this page"
	}
	return fmt.Sprintf("%d of %s%s %s", kept, rowsText(total), scope, existence(state, kept))
}

// existence is the tail of a sentence about the filters: what the subject did
// at the instant or in the window, in the tense the filter calls for and in
// the number the subject has.
func existence(state fleetState, subjects int) string {
	switch {
	case !state.Window && state.Pinned:
		return "existed at " + stamp(state.At)
	case !state.Window && subjects == 1:
		return "exists now"
	case !state.Window:
		return "exist now"
	case state.From != nil && state.To != nil:
		return "existed between " + stamp(*state.From) + " and " + stamp(*state.To)
	case state.From != nil:
		return "existed since " + stamp(*state.From)
	case state.To != nil:
		return "existed before " + stamp(*state.To)
	default:
		return "existed at some time"
	}
}
