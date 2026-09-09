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
// under one status, and of those rows the ones that existed over a window or
// at an instant. The status is the API's own filter, active by default. The
// window and the instant are the console's, applied to the rows the API
// served.
//
// The window is from and to, a half-open span: a resource existed in it when
// it was created before to and not deleted at or before from. The instant is
// at: a resource existed at it when it was created at or before it and not
// deleted at or before it. Without a window and without an instant the page
// shows what exists now, so it opens on what runs right now. With a window and
// no instant it shows everything that lived in the window. With an instant,
// whether inside a window or not, it shows what existed at that instant.
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
	fromParameter   = "from"
	toParameter     = "to"
	atParameter     = "at"
	// resourceTable is the name the resource listing's sort and filter
	// parameters carry.
	resourceTable = "resources"
)

// statuses are the parts of the fleet the API serves, in the order the switch
// prints them. The first is the API's default and the page's.
var statuses = []string{"active", "deleted", "all"}

// fleetState is what the request says about the fleet: which part of it, over
// which window, and at which instant. A nil bound is a window open on that
// side, and Pinned is false while no instant was named, which is now when
// there is no window either.
type fleetState struct {
	Status string
	From   *time.Time
	To     *time.Time
	At     time.Time
	Pinned bool
}

// windowed reports whether either bound of the window is set.
func (s fleetState) windowed() bool {
	return s.From != nil || s.To != nil
}

// readFleet reads the status, the window and the instant, each falling back
// to its default when absent. A status the API does not serve, a bound or an
// instant that does not parse, and a window that ends before it starts are
// refused the way any unreadable parameter is.
func readFleet(r *http.Request, now time.Time) (fleetState, error) {
	query := r.URL.Query()
	state := fleetState{Status: statuses[0], At: now}

	if status := query.Get(statusParameter); status != "" {
		if !slices.Contains(statuses, status) {
			return fleetState{}, &paramError{name: statusParameter, reason: "is not active, deleted or all"}
		}
		state.Status = status
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

	if at := query.Get(atParameter); at != "" {
		parsed, err := parseInstant(at)
		if err != nil {
			return fleetState{}, &paramError{name: atParameter, reason: "is not an instant: " + err.Error()}
		}
		state.At, state.Pinned = parsed, true
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
		return nil, &paramError{name: name, reason: "is not an instant: " + err.Error()}
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

// keep reports whether a row is shown: inside the window where one is set,
// and existing at the instant where one is pinned, or at now when neither is
// there.
func keep(resource httpapi.Resource, state fleetState, now time.Time) bool {
	if state.From != nil && resource.DeletedAt != nil && !resource.DeletedAt.After(*state.From) {
		return false
	}
	if state.To != nil && resource.CreatedAt != nil && !resource.CreatedAt.Before(*state.To) {
		return false
	}
	switch {
	case state.Pinned:
		return existedAt(resource, state.At)
	case state.windowed():
		return true
	default:
		return existedAt(resource, now)
	}
}

// existedAt reports whether a resource existed at an instant: created at or
// before it, and not deleted at or before it. A resource whose history shows
// no create has no created_at and is taken to have existed all along.
func existedAt(resource httpapi.Resource, at time.Time) bool {
	if resource.CreatedAt != nil && resource.CreatedAt.After(at) {
		return false
	}
	return resource.DeletedAt == nil || resource.DeletedAt.After(at)
}

// lifetimeHours is how long a resource has lived, from its creation to its
// deletion or to now for one still there, in hours at the two places the
// bills carry hours at. A resource whose history shows no create has no
// creation time, and reports that its lifetime is unknown.
func lifetimeHours(resource httpapi.Resource, now time.Time) (decimal.Decimal, bool) {
	if resource.CreatedAt == nil {
		return decimal.Zero, false
	}
	return hoursBetween(*resource.CreatedAt, lifetimeEnd(resource, now)), true
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

// unknownText is the line the resources page adds for the rows whose history
// starts without a create, and nothing when there is none.
func unknownText(rows int) string {
	switch rows {
	case 0:
		return ""
	case 1:
		return "1 row has no creation and no lifetime, because its history starts without a create"
	default:
		return fmt.Sprintf(
			"%d rows have no creation and no lifetime, because their histories start without a create", rows)
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
// switch, the three inputs with the hidden fields their form carries, the
// presets of the window and of the instant, and what the filters left of the
// page.
type fleetView struct {
	Path          string
	Hidden        []field
	Switch        []choice
	From          string
	To            string
	At            string
	WindowPresets []choice
	Presets       []choice
	Count         string
	// Empty is what the table says when the filters left nothing of a page
	// that held rows; a page the API served empty says what it always said.
	Empty string
	// Unknown counts the rows kept whose history starts without a create,
	// and is empty when there is none.
	Unknown string
}

// buildFleetView lays the controls out for one request. The status links drop
// the cursor, because a cursor positions a walk through one status and means
// nothing in another; the window and the instant keep it, because they filter
// the page the cursor named. The instant input is left empty while nothing is
// pinned, so that applying a window does not pin the instant to now on the
// way.
func buildFleetView(
	r *http.Request, state fleetState, now time.Time, kept, total, unknown int, paged bool,
) fleetView {
	values := r.URL.Query()
	view := fleetView{
		Path:    r.URL.Path,
		From:    inputValue(state.From),
		To:      inputValue(state.To),
		Unknown: unknownText(unknown),
	}
	if state.Pinned {
		view.At = state.At.UTC().Format(atLayout)
	}

	own := []string{fromParameter, toParameter, atParameter}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if slices.Contains(own, key) {
			continue
		}
		for _, value := range values[key] {
			if value != "" {
				view.Hidden = append(view.Hidden, field{Name: key, Value: value})
			}
		}
	}

	for _, status := range statuses {
		linked := maps.Clone(values)
		linked.Del("cursor")
		linked.Set(statusParameter, status)
		if status == statuses[0] {
			linked.Del(statusParameter)
		}
		view.Switch = append(view.Switch, choice{
			Label: status, Link: href(r.URL.Path, linked), Current: status == state.Status,
		})
	}

	view.WindowPresets = windowPresets(r.URL.Path, values, state, now)
	view.Presets = instantPresets(r.URL.Path, values, state, now)
	view.Count = fleetCount(state, kept, total, paged)
	if total > 0 && kept == 0 {
		view.Empty = "no resource of this page " + existence(state, 1)
	}
	return view
}

// inputValue is what a datetime-local input shows for a bound: the bound, or
// nothing for a side the window is open on.
func inputValue(bound *time.Time) string {
	if bound == nil {
		return ""
	}
	return bound.UTC().Format(atLayout)
}

// windowPresets are the windows one click away, four spans behind now that a
// demo month is read over, and all, which is no window at all.
func windowPresets(path string, values url.Values, state fleetState, now time.Time) []choice {
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

	var list []choice
	for _, span := range spans {
		linked := maps.Clone(values)
		linked.Set(fromParameter, stamp(span.from))
		linked.Set(toParameter, stamp(span.to))
		list = append(list, choice{
			Label:   span.label,
			Link:    href(path, linked),
			Current: state.From != nil && state.To != nil && state.From.Equal(span.from) && state.To.Equal(span.to),
		})
	}

	linked := maps.Clone(values)
	linked.Del(fromParameter)
	linked.Del(toParameter)
	return append(list, choice{Label: "all", Link: href(path, linked), Current: !state.windowed()})
}

// instantPresets are the instants one click away: now, which is no parameter
// at all and is offered while there is no window, four points behind it, and
// clear, which unpins the instant inside a window.
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
	var list []choice
	if !state.windowed() {
		list = append(list, choice{Label: "now", Link: href(path, unpinned), Current: !state.Pinned})
	}
	for _, point := range points {
		linked := maps.Clone(values)
		linked.Set(atParameter, stamp(point.at))
		list = append(list, choice{
			Label:   point.label,
			Link:    href(path, linked),
			Current: state.Pinned && state.At.Equal(point.at),
		})
	}
	if state.windowed() && state.Pinned {
		list = append(list, choice{Label: "clear", Link: href(path, unpinned)})
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
	case state.Pinned:
		return "existed at " + stamp(state.At)
	case state.From != nil && state.To != nil:
		return "existed between " + stamp(*state.From) + " and " + stamp(*state.To)
	case state.From != nil:
		return "existed since " + stamp(*state.From)
	case state.To != nil:
		return "existed before " + stamp(*state.To)
	case subjects == 1:
		return "exists now"
	default:
		return "exist now"
	}
}
