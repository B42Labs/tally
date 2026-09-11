// Package reporting is the demo console's read client for the Reporting API.
// It follows cmd/tally-vertical-slice/client.go: the same bearer header on
// every call, the same bounded body, and the same reading of a refused answer
// as the problem document the API answered with.
//
// The client issues nothing but GET. The console shows what the API already
// holds, so no call of this package can change a project, a resource, or an
// event.
//
// Every method returns the Request it made next to the model it decoded, which
// is what lets a page list where its numbers came from.
package reporting

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/reporting/httpapi"
)

const (
	// bodyLimit caps one answer. The API pages its lists and refuses an
	// aggregate too large rather than serving it truncated, so a body past this
	// is not an answer this client should keep reading into memory.
	bodyLimit = 64 << 20
	// requestTimeout bounds one call. A console page waits on it, so a server
	// that accepts the connection and then never answers has to end as a failed
	// request rather than as a page that never renders.
	requestTimeout = 30 * time.Second
)

// Client reads the Reporting API for the console. It is deliberately minimal:
// no retries, and one flat timeout rather than a policy, because a demo page
// that fails is reloaded by the reader looking at it.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds the read client. The base URL is the API's root without the
// /api/v1 suffix; one trailing slash is trimmed, so the two forms an operator
// writes address the same routes.
//
// The URL is taken as given. Whether it may carry the token is decided at
// startup by internal/console/config, which refuses one that is neither https
// nor loopback.
//
// An empty caFile trusts the system store. A named one is trusted instead of
// it, which is what reaches a dev cluster whose certificates come from a CA the
// host does not know.
func New(baseURL, token, caFile string) (*Client, error) {
	httpClient := &http.Client{
		Timeout: requestTimeout,
		// A redirect is answered rather than followed. The API documents none,
		// and Go strips the Authorization header only across hosts, so a
		// same-host redirect to http:// would carry the token over in the clear.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading the CA file %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		// A pool that accepted nothing trusts nothing, and every call would fail
		// with a verification error that says nothing about the file behind it.
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no PEM certificates were found in the CA file %s", caFile)
		}
		// The default transport is cloned rather than replaced: a bare literal
		// would drop the proxy, the dial timeout, and the handshake timeout it
		// sets, so naming a CA file would silently change how a call fails too.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
		httpClient.Transport = transport
	}

	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, http: httpClient}, nil
}

// Request is one call the client made: the method and the path, including the
// query string when one was sent. A page lists these as its provenance.
type Request struct {
	Method string
	Path   string
}

// ProblemError is an answer the API refused, as its problem document diagnosed
// it. The problem type is what a reader recognizes the case by, so it is kept
// next to the status code rather than dropped for it.
type ProblemError struct {
	Status int
	Type   string
	Title  string
	Detail string
}

// Error reports the status the API answered with and its own diagnosis. A
// problem document without a detail carries the title alone.
func (e *ProblemError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("the API answered %d %s: %s: %s", e.Status, e.Type, e.Title, e.Detail)
	}
	return fmt.Sprintf("the API answered %d %s: %s", e.Status, e.Type, e.Title)
}

// ProjectsQuery narrows a project list. Every field is optional, and an empty
// one filters nothing.
type ProjectsQuery struct {
	Platform   string
	Cloud      string
	ExternalID string
	Cursor     string
}

// RelationsQuery narrows a relation list. Every field is optional: an empty
// Direction or RelationType filters nothing, and a zero At is now.
type RelationsQuery struct {
	// Direction is incoming, outgoing or both, the values the route takes.
	Direction    string
	RelationType string
	At           time.Time
}

// ResourcesQuery narrows a resource list. Every field is optional, and an empty
// one filters nothing.
type ResourcesQuery struct {
	Cloud        string
	ProjectID    string
	ResourceType string
	State        string
	Status       string
	Cursor       string
	// Limit is how many rows one page carries at most, within the API's
	// bounds. Zero leaves the API's default.
	Limit int
}

// ListProjects reads one page of the registered projects. The page's
// NextCursor is returned as the API served it and never followed: one call
// answers one page, and asking for the next one is the caller's move.
func (c *Client) ListProjects(ctx context.Context, q ProjectsQuery) (httpapi.ProjectList, Request, error) {
	query := url.Values{}
	setFilter(query, "platform", q.Platform)
	setFilter(query, "cloud", q.Cloud)
	setFilter(query, "external_id", q.ExternalID)
	setFilter(query, "cursor", q.Cursor)

	var page httpapi.ProjectList
	request, err := c.get(ctx, withQuery("/api/v1/projects", query), &page)
	if err != nil {
		return httpapi.ProjectList{}, request, err
	}
	if err := refuseARepeatedCursor(page.NextCursor, q.Cursor); err != nil {
		return httpapi.ProjectList{}, request, err
	}
	return page, request, nil
}

// GetProject reads one project's registry row.
func (c *Client) GetProject(ctx context.Context, id uuid.UUID) (httpapi.Project, Request, error) {
	var project httpapi.Project
	request, err := c.get(ctx, projectRoute(id, ""), &project)
	if err != nil {
		return httpapi.Project{}, request, err
	}
	return project, request, nil
}

// ListProjectRelations reads the relations one project has at one instant,
// narrowed by q. A zero query reads them as they stand now, in both
// directions and of every type.
func (c *Client) ListProjectRelations(
	ctx context.Context, id uuid.UUID, q RelationsQuery,
) (httpapi.RelationList, Request, error) {
	query := url.Values{}
	setFilter(query, "direction", q.Direction)
	setFilter(query, "relation_type", q.RelationType)
	// The instant keeps its fraction rather than going through formatInstant:
	// the last instant of a billing period is a microsecond before the next one
	// starts, and rounded to the second it would miss a relation that began in
	// that second.
	if !q.At.IsZero() {
		query.Set("at", q.At.UTC().Format(time.RFC3339Nano))
	}

	var relations httpapi.RelationList
	request, err := c.get(ctx, withQuery(projectRoute(id, "/relations"), query), &relations)
	if err != nil {
		return httpapi.RelationList{}, request, err
	}
	return relations, request, nil
}

// ListRelatedProjects reads the projects a traversal from one project reaches.
func (c *Client) ListRelatedProjects(ctx context.Context, id uuid.UUID) (httpapi.RelatedProjectList, Request, error) {
	var related httpapi.RelatedProjectList
	request, err := c.get(ctx, projectRoute(id, "/related"), &related)
	if err != nil {
		return httpapi.RelatedProjectList{}, request, err
	}
	return related, request, nil
}

// GetProjectSummary reads what one project ran inside the half-open window
// [from, to). Both bounds are required by the route and travel as UTC.
func (c *Client) GetProjectSummary(
	ctx context.Context, id uuid.UUID, from, to time.Time,
) (httpapi.ProjectSummary, Request, error) {
	query := url.Values{
		"from": {formatInstant(from)},
		"to":   {formatInstant(to)},
	}

	var summary httpapi.ProjectSummary
	request, err := c.get(ctx, withQuery(projectRoute(id, "/summary"), query), &summary)
	if err != nil {
		return httpapi.ProjectSummary{}, request, err
	}
	return summary, request, nil
}

// ListResources reads one page of the current resources. As with ListProjects,
// the cursor is returned rather than followed.
func (c *Client) ListResources(ctx context.Context, q ResourcesQuery) (httpapi.ResourceList, Request, error) {
	query := url.Values{}
	setFilter(query, "cloud", q.Cloud)
	setFilter(query, "project_id", q.ProjectID)
	setFilter(query, "resource_type", q.ResourceType)
	setFilter(query, "state", q.State)
	setFilter(query, "status", q.Status)
	setFilter(query, "cursor", q.Cursor)
	if q.Limit > 0 {
		query.Set("limit", strconv.Itoa(q.Limit))
	}

	var page httpapi.ResourceList
	request, err := c.get(ctx, withQuery("/api/v1/resources", query), &page)
	if err != nil {
		return httpapi.ResourceList{}, request, err
	}
	if err := refuseARepeatedCursor(page.NextCursor, q.Cursor); err != nil {
		return httpapi.ResourceList{}, request, err
	}
	return page, request, nil
}

// GetLifecycle reads one resource, its event history, and the billable
// intervals that history folds into.
func (c *Client) GetLifecycle(
	ctx context.Context, cloud, resourceType, resourceID string,
) (httpapi.Lifecycle, Request, error) {
	route := "/api/v1/resources/" + url.PathEscape(cloud) +
		"/" + url.PathEscape(resourceType) +
		"/" + url.PathEscape(resourceID) + "/lifecycle"

	var lifecycle httpapi.Lifecycle
	request, err := c.get(ctx, route, &lifecycle)
	if err != nil {
		return httpapi.Lifecycle{}, request, err
	}
	return lifecycle, request, nil
}

// ResourceStats counts the current resources along the dimensions groupBy
// names. An empty status leaves the API's own default in place.
//
// The at parameter of the route is never sent: the API answers 501 to a
// grouping asked for at an instant, so what the console shows is always the
// count the projection holds now.
func (c *Client) ResourceStats(
	ctx context.Context, groupBy []string, status string,
) (httpapi.ResourceStatsList, Request, error) {
	query := url.Values{"group_by": {strings.Join(groupBy, ",")}}
	setFilter(query, "status", status)

	var stats httpapi.ResourceStatsList
	request, err := c.get(ctx, withQuery("/api/v1/stats/resources", query), &stats)
	if err != nil {
		return httpapi.ResourceStatsList{}, request, err
	}
	return stats, request, nil
}

// EventStats counts the stored events of the half-open window [from, to) per
// bucket of the given interval. The route requires all four parameters.
func (c *Client) EventStats(
	ctx context.Context, groupBy []string, interval string, from, to time.Time,
) (httpapi.EventStatsList, Request, error) {
	query := url.Values{
		"group_by": {strings.Join(groupBy, ",")},
		"interval": {interval},
		"from":     {formatInstant(from)},
		"to":       {formatInstant(to)},
	}

	var stats httpapi.EventStatsList
	request, err := c.get(ctx, withQuery("/api/v1/stats/events", query), &stats)
	if err != nil {
		return httpapi.EventStatsList{}, request, err
	}
	return stats, request, nil
}

// ListRejectedEvents reads the ingest items the API refused. A limit of zero or
// less is left out, which leaves the page size the API decides in place.
func (c *Client) ListRejectedEvents(ctx context.Context, limit int) (httpapi.DeadLetterList, Request, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}

	var page httpapi.DeadLetterList
	request, err := c.get(ctx, withQuery("/api/v1/rejected-events", query), &page)
	if err != nil {
		return httpapi.DeadLetterList{}, request, err
	}
	return page, request, nil
}

// get runs one authenticated GET and decodes its answer. The token travels in
// the header on every call, so nothing the API serves is read
// unauthenticated. The Request comes back even from a failed call: a page
// naming what it asked for is what makes an error readable.
func (c *Client) get(ctx context.Context, path string, out any) (Request, error) {
	request := Request{Method: http.MethodGet, Path: path}
	endpoint := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return request, fmt.Errorf("building the request for %s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return request, fmt.Errorf("calling %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return request, problemError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, bodyLimit)).Decode(out); err != nil {
		return request, fmt.Errorf("decoding the answer of %s: %w", endpoint, err)
	}
	return request, nil
}

// problemError turns a refused answer into an error carrying the API's own
// diagnosis. A body that is no problem document at all, the HTML of a gateway
// between the console and the API for example, is reported as such rather than
// as a problem with an empty title.
func problemError(resp *http.Response) error {
	var problem struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, bodyLimit)).Decode(&problem); err != nil {
		return fmt.Errorf("the API answered %d with an unreadable problem document: %w", resp.StatusCode, err)
	}

	return &ProblemError{
		Status: resp.StatusCode,
		Type:   problem.Type,
		Title:  problem.Title,
		Detail: problem.Detail,
	}
}

// projectRoute addresses one project, or one of the routes hanging off it. The
// id is escaped like every other path segment, so a caller cannot widen the
// route by what it passes.
func projectRoute(id uuid.UUID, suffix string) string {
	return "/api/v1/projects/" + url.PathEscape(id.String()) + suffix
}

// withQuery assembles one route with its query string. A call that set no
// parameter addresses the bare route rather than one ending in a lone question
// mark.
func withQuery(route string, query url.Values) string {
	if len(query) == 0 {
		return route
	}
	return route + "?" + query.Encode()
}

// setFilter adds one filter when the caller set it. An empty value is left out
// rather than sent as an empty parameter, so a read is narrowed by the filters
// the caller named and by nothing else.
func setFilter(query url.Values, name, value string) {
	if value != "" {
		query.Set(name, value)
	}
}

// formatInstant writes one window bound the way the routes take it. The bound
// travels as UTC, so the window a page asks for does not depend on the zone the
// time it was built from carried.
func formatInstant(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// refuseARepeatedCursor stops a walk the API cannot end. A page naming the
// cursor it was given would be asked for again forever, with the same rows
// appended on every turn, so it is refused rather than shown.
func refuseARepeatedCursor(next *string, cursor string) error {
	if next != nil && *next == cursor {
		return fmt.Errorf("the API repeated the cursor %q, which would page forever", cursor)
	}
	return nil
}
