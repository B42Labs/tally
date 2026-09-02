package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

// The push side of the metrics: the samples a month places go to an OTLP/HTTP
// endpoint as JSON, the route the Gateway publishes in front of the collector.
//
// The document is written here with encoding/json rather than handed to the
// OpenTelemetry Go SDK. The SDK stamps a point with the instant it collected
// it, and every point of a month belongs to a virtual instant that is months
// away from the wall clock. There is no seam in an SDK exporter to hand that
// instant to, and a hand-written document is the shorter way to a point that
// carries the instant it belongs to.

const (
	// maxDataPoints is the cap one request carries. A run paced by a factor of 0
	// renders a whole month at once, and the loop that pushes it flushes at this
	// count rather than posting a million points in a single body.
	maxDataPoints = 5000
	// pushTimeout bounds one attempt. The retries happen below http.Client.Do,
	// so the bound is per attempt rather than per batch.
	pushTimeout = 30 * time.Second
	// The waits between two attempts of one batch.
	pushRetryWaitMin = 500 * time.Millisecond
	pushRetryWaitMax = 5 * time.Second
	// otlpCumulative is the AggregationTemporality enum value for cumulative,
	// which is what the counters of a month report: the value of a sample is
	// everything that accrued before it.
	otlpCumulative = 2
)

// The OTLP/HTTP JSON document, as much of it as a push writes: one resource
// carrying one scope, and one metric object per series name below it.
//
// No type here carries a unit member. The prometheusremotewrite exporter of the
// collector appends _total to a monotonic cumulative sum whose name does not
// already end in it, and a unit suffix to a metric that declares a unit. A name
// that already ends in _total, on a metric with no unit, is therefore stored
// under exactly the name that was sent, whichever way those two options are
// set.
type otlpExport struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpResourceMetrics struct {
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpScopeMetrics struct {
	Metrics []otlpMetric `json:"metrics"`
}

// otlpMetric is one series name with its points. Exactly one of the two members
// stands: a gauge is a value read at an instant, a sum is a counter.
type otlpMetric struct {
	Name  string     `json:"name"`
	Gauge *otlpGauge `json:"gauge,omitempty"`
	Sum   *otlpSum   `json:"sum,omitempty"`
}

type otlpGauge struct {
	DataPoints []otlpPoint `json:"dataPoints"`
}

type otlpSum struct {
	AggregationTemporality int         `json:"aggregationTemporality"`
	IsMonotonic            bool        `json:"isMonotonic"`
	DataPoints             []otlpPoint `json:"dataPoints"`
}

// otlpPoint is one value of one series at one instant.
//
// The instant and the value are JSON strings, which is the proto3 JSON mapping
// of a 64-bit integer and what the OTLP/HTTP receiver reads them as. The
// recorded drill in docs/grafana-dashboards.md sends timeUnixNano as a string
// too. An integer value also keeps a byte count exact, which a float64 stops
// doing above 2^53.
type otlpPoint struct {
	Attributes   []otlpAttribute `json:"attributes"`
	TimeUnixNano string          `json:"timeUnixNano"`
	AsInt        string          `json:"asInt"`
}

type otlpAttribute struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue string `json:"stringValue"`
}

// encodeBatch renders the samples as one OTLP/HTTP JSON document: one metric
// object per series name, in the order the names were first seen, holding the
// points of that name.
//
// The attributes of a point are the static labels overlaid by the sample's own,
// so the sample wins a clash, sorted by key. A sample that carries no labels of
// its own is one of an aggregate series and carries the static ones alone.
//
// Two samples of one name that disagree on their kind are refused. A series is
// a gauge or a sum, and a document stating it as both stores it under whichever
// of the two the collector read last.
func encodeBatch(samples []Sample, static map[string]string) ([]byte, error) {
	metrics := make([]otlpMetric, 0)
	index := make(map[string]int, len(samples))

	for _, sample := range samples {
		at, ok := index[sample.Name]
		if !ok {
			metric := otlpMetric{Name: sample.Name}
			if sample.Kind == KindCounter {
				metric.Sum = &otlpSum{AggregationTemporality: otlpCumulative, IsMonotonic: true}
			} else {
				metric.Gauge = &otlpGauge{}
			}
			metrics = append(metrics, metric)
			at = len(metrics) - 1
			index[sample.Name] = at
		}

		point := otlpPoint{
			Attributes:   attributesOf(sample.Labels, static),
			TimeUnixNano: strconv.FormatInt(sample.At.UnixNano(), 10),
			AsInt:        strconv.FormatInt(sample.Value, 10),
		}
		switch {
		case sample.Kind == KindCounter && metrics[at].Sum != nil:
			metrics[at].Sum.DataPoints = append(metrics[at].Sum.DataPoints, point)
		case sample.Kind == KindGauge && metrics[at].Gauge != nil:
			metrics[at].Gauge.DataPoints = append(metrics[at].Gauge.DataPoints, point)
		default:
			return nil, fmt.Errorf(
				"the metric %s carries a gauge sample and a counter sample in one batch", sample.Name)
		}
	}

	return json.Marshal(otlpExport{ResourceMetrics: []otlpResourceMetrics{{
		ScopeMetrics: []otlpScopeMetrics{{Metrics: metrics}},
	}}})
}

// attributesOf merges the labels of a sample over the static ones and states
// them as OTLP attributes, sorted by key. Sorting them makes one batch of a
// series byte-identical to the next one over the same samples, which a diff of
// two runs reads.
func attributesOf(labels, static map[string]string) []otlpAttribute {
	merged := make(map[string]string, len(static)+len(labels))
	maps.Copy(merged, static)
	maps.Copy(merged, labels)

	attributes := make([]otlpAttribute, 0, len(merged))
	for _, key := range slices.Sorted(maps.Keys(merged)) {
		attributes = append(attributes, otlpAttribute{Key: key, Value: otlpValue{StringValue: merged[key]}})
	}
	return attributes
}

// Pusher posts samples to an OTLP/HTTP metrics endpoint under a basic
// credential.
//
// It never formats the password or the Authorization header into an error or a
// log line. What a failure states is the url, the status, and the beginning of
// the body the endpoint refused with, which is what an operator needs and where
// the credential never is. The url an error carries is the redacted one: a
// credential a caller put into the URL itself is a password as much as the
// configured one is.
type Pusher struct {
	url      string
	safeURL  string
	user     string
	password string
	static   map[string]string
	client   *http.Client
}

// NewPusher returns the pusher that posts to url under the credential user and
// password. Every point it sends carries platform and cloud on top of the
// labels of its sample: a push has no scrape job to take those two from, and
// the dashboards filter on both.
//
// A nil httpClient selects the package default, which is the one
// internal/engine/counters builds for its queries. The retrying transport sits
// below http.Client.Do, so a push makes one call and sees one result however
// many attempts it took. The default policy retries connection errors, a 429,
// which is what the Gateway's BackendTrafficPolicy answers a run that pushes
// faster than the rate it allows, and a 5xx except 501. A 4xx such as the 401
// of a wrong password is returned at once. Tests pass their own client.
func NewPusher(url, user, password, cloud string, httpClient *http.Client) *Pusher {
	if httpClient == nil {
		rc := retryablehttp.NewClient()
		// The default logger writes every retry to stderr, past the run's handler.
		rc.Logger = nil
		// Without a passthrough handler an exhausted retry on a 5xx returns a bare
		// "giving up after N attempt(s)" error and drops the response, and the
		// status the endpoint refused with is what the message needs.
		rc.ErrorHandler = retryablehttp.PassthroughErrorHandler
		rc.RetryWaitMin = pushRetryWaitMin
		rc.RetryWaitMax = pushRetryWaitMax
		rc.HTTPClient.Timeout = pushTimeout
		// RetryMax stays at its default of four.
		httpClient = rc.StandardClient()
	}
	return &Pusher{
		url:      url,
		safeURL:  redactedURL(url),
		user:     user,
		password: password,
		static:   map[string]string{"platform": platformOpenStack, "cloud": cloud},
		client:   httpClient,
	}
}

// redactedURL is the endpoint an error may name, and a URL that does not parse
// is not named at all. Neither half of the userinfo survives: an endpoint may
// carry a bearer token as its user, and url.URL.Redacted replaces the password
// alone. The query goes whole, because an api key is as often a parameter as it
// is a password, and the host and the path are what name the endpoint an
// operator mistyped.
//
// ValidateMetrics refuses all three shapes, so a run never reaches here with
// one. This is the second line, for a Pusher built straight from a URL: the
// error it formats is logged as JSON to stdout and shipped from there.
func redactedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "the configured endpoint"
	}
	if parsed.User != nil {
		parsed.User = url.User("redacted")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// Push posts the samples in chunks of at most maxDataPoints points and stops at
// the first chunk that fails, because a batch the endpoint refused is one the
// ones behind it will be refused with too. An empty batch is no request at all.
//
// A cancelled context comes back as the context's own error, so a caller's
// errors.Is(err, context.Canceled) holds and the publishing loop reads it as
// the clean stop a signal asked for rather than as a failed push.
func (p *Pusher) Push(ctx context.Context, samples []Sample) error {
	for chunk := range slices.Chunk(samples, maxDataPoints) {
		if err := p.send(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

// send posts one batch and reports what the endpoint answered.
func (p *Pusher) send(ctx context.Context, samples []Sample) error {
	body, err := encodeBatch(samples, p.static)
	if err != nil {
		return fmt.Errorf("pushing metrics to %s: %w", p.safeURL, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pushing metrics to %s: %w", p.safeURL, err)
	}
	req.SetBasicAuth(p.user, p.password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		// A run that was stopped did not fail its push. The context's error is
		// returned as it is, rather than wrapped in what the transport made of it,
		// so that the caller reads its own cancellation back out.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The endpoint is named once, redacted, and the cause is unwrapped out of
		// what carried it. http.Client reports a transport failure inside a
		// *url.Error holding the request's own URL, and the masking it does there
		// covers the password half of a userinfo alone: a bare user and the whole
		// query stand in the message it formats.
		var wrapped *url.Error
		if errors.As(err, &wrapped) {
			err = wrapped.Err
		}
		return fmt.Errorf("pushing metrics to %s: %w", p.safeURL, err)
	}
	// Reading the rest of an accepted answer before closing it is what lets the
	// connection carry the batch that follows, of which a month has many.
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		// What the endpoint refused with is what an operator needs: the collector
		// names the point it rejected there. A read that failed halfway still
		// returns what it got, which is what the message quotes.
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, refusalBodyMax))
		return fmt.Errorf("pushing metrics to %s: unexpected status %d: %s",
			p.safeURL, resp.StatusCode, excerpt)
	}
	return nil
}
