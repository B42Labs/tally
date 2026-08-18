package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/b42labs/tally/internal/core/event"
)

const (
	// pageLimit is how many resources one page of the resource list carries. It
	// is the maximum the API serves, which keeps the walk to as few calls as the
	// contract allows.
	pageLimit = 1000
	// pageBudget caps how many pages the resource walk reads. An API that keeps
	// handing out cursors beyond it is broken or hostile, and either way the run
	// stops rather than calling forever.
	pageBudget = 1000
	// resourceBudget caps what the walk collects. limit= is a request, not a
	// constraint: a server free to ignore it can serve a bodyLimit-sized page
	// every time, and pageBudget bounds the number of those pages without
	// bounding their contents. A million instances is past any project this
	// prototype rates.
	resourceBudget = pageBudget * pageLimit
	// bodyLimit caps one answer. The API's pages are bounded by pageLimit, so a
	// body past this is not an answer this client should keep reading into
	// memory.
	bodyLimit = 64 << 20
	// requestTimeout bounds one call. A server that accepts the connection and
	// then never answers would otherwise hold the run until the operator
	// interrupts it.
	requestTimeout = 2 * time.Minute
)

// client reads the Reporting API. It is deliberately minimal: no retries, and
// one flat timeout rather than a policy, because a prototype run that fails is
// rerun by the operator watching it.
type client struct {
	baseURL string
	token   string
	http    *http.Client
}

// newClient builds the read client. The base URL is the API's root without the
// /api/v1 suffix, the same form the collector's sender takes.
//
// An empty caFile trusts the system store. A named one is trusted instead of
// it, which is what reaches a dev cluster whose certificates come from a CA the
// host does not know.
func newClient(baseURL, token, caFile string) (*client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("reading the reporting URL %s: %w", baseURL, err)
	}
	// The token rides on every call, so a plaintext base URL puts a read_all
	// credential on the wire in the clear. Loopback is exempt: that is where a
	// test's server and a port-forwarded cluster listen.
	if parsed.Scheme != "https" && !isLoopback(parsed.Hostname()) {
		return nil, fmt.Errorf("the reporting URL %s is not https: the API token would travel in cleartext", baseURL)
	}

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
		// sets, so naming a CA file would silently change how a run fails too.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
		httpClient.Transport = transport
	}

	return &client{baseURL: baseURL, token: token, http: httpClient}, nil
}

// isLoopback reports whether a host names the machine the run is on. The name
// is taken as well as the address, because that is what a port-forward is
// reached under.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listResources walks the projection for one project's instances and returns
// their ids in the order the API served them. Deleted resources are asked for
// as well: one that was deleted during the month still billed the part of it it
// lived through.
func (c *client) listResources(ctx context.Context, cloud, project string) ([]string, error) {
	var (
		ids    []string
		cursor string
	)

	for page := 0; ; page++ {
		if page == pageBudget {
			return nil, fmt.Errorf("the resource list of %s/%s did not end within %d pages",
				cloud, project, pageBudget)
		}

		query := url.Values{
			"cloud":         {cloud},
			"project_id":    {project},
			"resource_type": {"instance"},
			"status":        {"all"},
			"limit":         {strconv.Itoa(pageLimit)},
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}

		var answer struct {
			Items []struct {
				ResourceID string `json:"resource_id"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := c.get(ctx, c.baseURL+"/api/v1/resources?"+query.Encode(), &answer); err != nil {
			return nil, err
		}

		for _, item := range answer.Items {
			if len(ids) == resourceBudget {
				return nil, fmt.Errorf("the resource list of %s/%s served more than %d resources",
					cloud, project, resourceBudget)
			}
			ids = append(ids, item.ResourceID)
		}
		if answer.NextCursor == nil {
			return ids, nil
		}
		// A cursor that does not advance would be asked for again forever, with
		// the same page appended to the result on every turn.
		if *answer.NextCursor == cursor {
			return nil, fmt.Errorf("the API repeated the cursor %q, which would page forever", cursor)
		}
		cursor = *answer.NextCursor
	}
}

// fetchHistory reads one instance's whole event history. The endpoint never
// pages: it either serves the history in one answer or refuses a history longer
// than it will serve, which the error names the resource for.
func (c *client) fetchHistory(ctx context.Context, cloud, resourceID string) ([]event.Stored, error) {
	endpoint := fmt.Sprintf("%s/api/v1/resources/%s/instance/%s/events",
		c.baseURL, url.PathEscape(cloud), url.PathEscape(resourceID))

	var page struct {
		Items []event.Stored `json:"items"`
	}
	if err := c.get(ctx, endpoint, &page); err != nil {
		return nil, fmt.Errorf("fetching the history of %s: %w", resourceID, err)
	}
	return page.Items, nil
}

// get runs one authenticated GET and decodes its answer. The token travels in
// the header on every call, so nothing the API serves is read unauthenticated.
func (c *client) get(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("building the request for %s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return problemError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, bodyLimit)).Decode(out); err != nil {
		return fmt.Errorf("decoding the answer of %s: %w", endpoint, err)
	}
	return nil
}

// problemError turns a refused answer into an error carrying the API's own
// diagnosis. The problem type is what a reader recognizes the case by, so it is
// in the text next to the title rather than dropped for the status code.
func problemError(resp *http.Response) error {
	var problem struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, bodyLimit)).Decode(&problem); err != nil {
		return fmt.Errorf("the API answered %d with an unreadable problem document: %w", resp.StatusCode, err)
	}

	if problem.Detail != "" {
		return fmt.Errorf("the API answered %d %s: %s: %s",
			resp.StatusCode, problem.Type, problem.Title, problem.Detail)
	}
	return fmt.Errorf("the API answered %d %s: %s", resp.StatusCode, problem.Type, problem.Title)
}
