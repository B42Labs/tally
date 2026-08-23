package counters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/shopspring/decimal"
)

// queryTimeout bounds one attempt of an instant query. Retries happen below
// http.Client.Do, so the bound is per attempt rather than per query.
const queryTimeout = 30 * time.Second

// bodyExcerpt is how much of a body that is not a VictoriaMetrics error
// document an error message quotes, so that an HTML error page from something
// in front of VictoriaMetrics does not end up in the log line whole.
const bodyExcerpt = 200

// maxBodyBytes bounds what one answer reads into memory. The single-series
// check below rejects a query that does not aggregate, but only once the whole
// body is read and decoded, which is too late for the vector of a hundred
// thousand series such a query returns.
const maxBodyBytes = 1 << 20

// ErrAnswerShape marks the failures of a query that are a property of the one
// answer it got rather than of the store: a vector that did not aggregate, a
// value MetricsQL printed as NaN or Inf, a result of the wrong type. A Querier
// wraps it so that a caller can tell them apart from an outage, because the
// next resource's answer says nothing about them and the retry ladder an outage
// costs was never paid for them.
var ErrAnswerShape = errors.New("the answer cannot be billed")

// VMClient is the instant-query client over the VictoriaMetrics read endpoint
// /api/v1/query.
type VMClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewVMClient checks baseURL and returns the client that queries it. The url is
// the read endpoint's base, with or without a trailing slash and with or
// without a path prefix, such as http://victoriametrics:8428.
//
// A nil httpClient selects the package default, which retries connection
// errors, 429, and 5xx below http.Client.Do and bounds one attempt by
// queryTimeout. Tests pass their own client.
func NewVMClient(baseURL string, httpClient *http.Client) (*VMClient, error) {
	if baseURL == "" {
		return nil, errors.New("the VictoriaMetrics url is empty")
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing the VictoriaMetrics url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the VictoriaMetrics url %q must be http or https with a host", baseURL)
	}

	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	return &VMClient{baseURL: baseURL, httpClient: httpClient}, nil
}

// defaultHTTPClient is the client a caller gets when it passes none. The
// retrying transport sits below http.Client.Do, so Query makes one call and
// sees one result however many attempts it took.
func defaultHTTPClient() *http.Client {
	rc := retryablehttp.NewClient()
	// The default logger writes every retry to stderr, past the engine's
	// handler.
	rc.Logger = nil
	// Without a passthrough handler an exhausted retry on a 5xx returns a bare
	// "giving up after N attempt(s)" error and drops the response. With it the
	// last response comes back and Query reports its status.
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler
	rc.HTTPClient.Timeout = queryTimeout
	// RetryMax stays at its default of four. DefaultRetryPolicy retries
	// connection errors, 429, and 5xx except 501; a 4xx such as the 422
	// VictoriaMetrics answers for an invalid query is returned at once.
	return rc.StandardClient()
}

// vmResponse is the part of an /api/v1/query answer this package reads. A
// series carries its value as a two-element array of the sample's timestamp and
// its value, and the value is a string rather than a JSON number so that it
// arrives with the precision VictoriaMetrics printed it at.
type vmResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value [2]json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Query runs expr as an instant query at at, which is a draft's end, and
// returns the single value it selects. The value comes back unrounded, because
// the quantity a counter contributes is rounded where it is merged into the
// draft.
//
// An empty result is zero: a resource that reported no sample over the query's
// window used none of the metric. More than one series is an error rather than
// a sum or a pick of the first, because both would bill a number nobody
// configured; such a query has to aggregate to a single series.
func (c *VMClient) Query(ctx context.Context, expr string, at time.Time) (decimal.Decimal, error) {
	u := strings.TrimRight(c.baseURL, "/") + "/api/v1/query?" + url.Values{
		"query": {expr},
		"time":  {at.UTC().Format(time.RFC3339Nano)},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return decimal.Zero, fmt.Errorf("querying VictoriaMetrics: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return decimal.Zero, fmt.Errorf("querying VictoriaMetrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return decimal.Zero, fmt.Errorf("querying VictoriaMetrics: %w", err)
	}

	var r vmResponse
	decodeErr := json.Unmarshal(body, &r)

	// VictoriaMetrics reports a rejected query as an error document, but what
	// sits in front of it answers in whatever form it likes, so a body that
	// does not decode is quoted instead of dropped.
	if resp.StatusCode != http.StatusOK {
		if decodeErr != nil {
			excerpt := body
			if len(excerpt) > bodyExcerpt {
				excerpt = excerpt[:bodyExcerpt]
			}
			return decimal.Zero, fmt.Errorf("VictoriaMetrics answered status %d: %s", resp.StatusCode, excerpt)
		}
		return decimal.Zero, fmt.Errorf("VictoriaMetrics answered status %d %s: %s", resp.StatusCode, r.ErrorType, r.Error)
	}

	// The bound is about the vector a query that does not aggregate returns, so
	// it is read as that only once the answer is one: an error page from in
	// front of VictoriaMetrics is as large as it likes and is reported by its
	// status above.
	if len(body) > maxBodyBytes {
		return decimal.Zero, fmt.Errorf(
			"%w: the VictoriaMetrics answer is larger than %d bytes: the query must aggregate to a single series",
			ErrAnswerShape, maxBodyBytes)
	}

	if decodeErr != nil {
		return decimal.Zero, fmt.Errorf("decoding the VictoriaMetrics response: %w", decodeErr)
	}
	if r.Status != "success" {
		return decimal.Zero, fmt.Errorf("VictoriaMetrics answered status 200 %s: %s", r.ErrorType, r.Error)
	}
	if r.Data.ResultType != "vector" {
		return decimal.Zero, fmt.Errorf("%w: VictoriaMetrics returned a %s result, want a vector",
			ErrAnswerShape, r.Data.ResultType)
	}

	switch len(r.Data.Result) {
	case 0:
		return decimal.Zero, nil
	case 1:
	default:
		return decimal.Zero, fmt.Errorf(
			"%w: VictoriaMetrics returned %d series, want at most one: the query must aggregate to a single series",
			ErrAnswerShape, len(r.Data.Result))
	}

	var s string
	if err := json.Unmarshal(r.Data.Result[0].Value[1], &s); err != nil {
		return decimal.Zero, fmt.Errorf("decoding the VictoriaMetrics response: %w", err)
	}
	// NaN, +Inf and -Inf, which MetricsQL prints for a division by zero and for
	// an overflow, do not parse and are reported rather than billed.
	value, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%w: parsing the VictoriaMetrics value %q: %w", ErrAnswerShape, s, err)
	}
	return value, nil
}

// Window renders the {window} placeholder from a draft's Seconds, which are
// whole seconds. The unit is the coarsest one that divides them, so a draft
// covering a month of 30 days is 360h rather than 1296000s.
func Window(seconds int64) string {
	switch {
	// Two state transitions inside one second leave a draft shorter than a
	// second, and it still has to be measured over a window MetricsQL accepts:
	// a second is the shortest lookbehind there is, while [0s] is either
	// rejected, which would fail the run, or answered with something nobody
	// chose.
	case seconds < 1:
		return "1s"
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	case seconds%60 == 0:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

// inertIdentity is what a substituted identity value may hold. MetricsQL writes
// string literals in double quotes, single quotes and backticks, and a
// placeholder may sit outside a literal altogether, so escaping for one of
// those forms would leave the others open. These characters carry no meaning in
// any of them: they can neither end a literal nor start an expression.
//
// They are inert as a literal, not as a pattern: a dot is a wildcard where a
// value is read as a regular expression, so validate keeps a substituted
// identity out of a =~ or !~ matcher.
var inertIdentity = regexp.MustCompile(`^[A-Za-z0-9._:/@-]*$`)

// RenderQuery substitutes a metricsql source's placeholders with the draft they
// are measured for: {cloud}, {resource_id} and {project_id} with the draft's
// identity, {window} with its length.
//
// The three identity values reach the engine from ingested event data, while
// the query around them is text an operator wrote. A value that is not inert in
// MetricsQL is therefore refused rather than escaped: nothing here can tell
// which of MetricsQL's string literal forms a placeholder sits in, or whether
// it sits in one at all, so an escaper for any single form would let such a
// value compose a query of its own. Only the placeholders the query uses are
// checked. The window is rendered from a count of seconds and needs no check.
func RenderQuery(query, cloud, resourceID, projectID string, seconds int64) (string, error) {
	for _, identity := range []struct{ name, value string }{
		{"cloud", cloud}, {"resource_id", resourceID}, {"project_id", projectID},
	} {
		if !strings.Contains(query, "{"+identity.name+"}") {
			continue
		}
		if !inertIdentity.MatchString(identity.value) {
			return "", fmt.Errorf("the %s %q holds a character a MetricsQL query may not carry",
				identity.name, identity.value)
		}
	}

	return strings.NewReplacer(
		"{cloud}", cloud,
		"{resource_id}", resourceID,
		"{project_id}", projectID,
		"{window}", Window(seconds),
	).Replace(query), nil
}
