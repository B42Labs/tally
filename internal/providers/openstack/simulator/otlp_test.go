package simulator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

// The credential the cases push under. The password is a literal the refusal
// case searches the error for: a message that carried it would put it into
// whatever an operator pastes it into.
const (
	pushUser     = "tally"
	pushPassword = "s3cret-of-the-test"
)

// pushInstant is the virtual instant the pushed points belong to.
var pushInstant = time.Date(2026, 7, 1, 10, 5, 0, 0, time.UTC)

// pushClient builds the client a case pushes through: the retry policy of the
// package default, with the waits cut to a millisecond and the number of
// retries set per case.
func pushClient(retryMax int) *http.Client {
	rc := retryablehttp.NewClient()
	rc.Logger = nil
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler
	rc.RetryWaitMin = time.Millisecond
	rc.RetryWaitMax = time.Millisecond
	rc.RetryMax = retryMax
	return rc.StandardClient()
}

// pushedRequest is what the stand-in endpoint saw of one request.
type pushedRequest struct {
	authorization string
	contentType   string
	raw           []byte
	export        otlpExport
}

// otlpServer stands in for the OTLP/HTTP endpoint. It answers by the statuses
// the case scripted, the last of them repeated for every request past their
// number, so a case states a refusal or a retry ladder and holds the requests
// it took against it afterwards.
type otlpServer struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []int
	requests []pushedRequest
}

func newOTLPServer(t *testing.T, statuses ...int) *otlpServer {
	t.Helper()

	server := &otlpServer{statuses: statuses}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the pushed body: %v", err)
		}
		var export otlpExport
		if err := json.Unmarshal(raw, &export); err != nil {
			t.Errorf("the pushed body %q is no OTLP document: %v", raw, err)
		}

		server.mu.Lock()
		server.requests = append(server.requests, pushedRequest{
			authorization: r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
			raw:           raw,
			export:        export,
		})
		status := http.StatusOK
		if len(server.statuses) > 0 {
			status = server.statuses[min(len(server.requests)-1, len(server.statuses)-1)]
		}
		server.mu.Unlock()

		if status/100 != 2 {
			http.Error(w, "the collector refused the batch", status)
			return
		}
		// An accepted push is answered with the empty export response.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(server.Close)
	return server
}

// seen is the requests the endpoint took. A retried batch reaches the handler
// from the client's goroutine and a case reads the slice from its own, so it is
// guarded and handed out as a copy.
func (s *otlpServer) seen() []pushedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// pusherTo builds the pusher of a case.
func pusherTo(server *otlpServer, retryMax int) *Pusher {
	return NewPusher(server.URL, pushUser, pushPassword, testCloud, pushClient(retryMax))
}

// closedEndpoint is an address on the loopback nothing listens on: a port is
// taken and handed back at once. A push to it fails in the transport, before
// there is a status to read.
func closedEndpoint(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("taking a port to close again: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("closing the port %s again: %v", address, err)
	}
	return "http://" + address + "/v1/metrics"
}

// countSample is the batch the transport cases push. What they hold is the
// client rather than the document, so one point of one series is enough.
func countSample() []Sample {
	return []Sample{{Name: seriesNovaTotalVMs, Value: 3, At: pushInstant, Kind: KindGauge}}
}

// decodeExport reads back a document encodeBatch wrote.
func decodeExport(t *testing.T, body []byte) otlpExport {
	t.Helper()

	var export otlpExport
	if err := json.Unmarshal(body, &export); err != nil {
		t.Fatalf("decoding the document %s: %v", body, err)
	}
	return export
}

// metricsOf unwraps the one resource and the one scope of a document.
func metricsOf(t *testing.T, export otlpExport) []otlpMetric {
	t.Helper()

	if len(export.ResourceMetrics) != 1 {
		t.Fatalf("the document carries %d resourceMetrics, want 1", len(export.ResourceMetrics))
	}
	if len(export.ResourceMetrics[0].ScopeMetrics) != 1 {
		t.Fatalf("the resource carries %d scopeMetrics, want 1",
			len(export.ResourceMetrics[0].ScopeMetrics))
	}
	return export.ResourceMetrics[0].ScopeMetrics[0].Metrics
}

// pointsOf is the points of a metric, whichever of the two members carries
// them.
func pointsOf(metric otlpMetric) []otlpPoint {
	switch {
	case metric.Sum != nil:
		return metric.Sum.DataPoints
	case metric.Gauge != nil:
		return metric.Gauge.DataPoints
	default:
		return nil
	}
}

// onePointOf is the single point a metric of these cases carries.
func onePointOf(t *testing.T, metric otlpMetric) otlpPoint {
	t.Helper()

	points := pointsOf(metric)
	if len(points) != 1 {
		t.Fatalf("%s carries %d points, want 1", metric.Name, len(points))
	}
	return points[0]
}

// attributeKeys is the keys of a point in the order they were sent.
func attributeKeys(point otlpPoint) []string {
	keys := make([]string, 0, len(point.Attributes))
	for _, attribute := range point.Attributes {
		keys = append(keys, attribute.Key)
	}
	return keys
}

// attributeValues is the attributes of a point by key.
func attributeValues(point otlpPoint) map[string]string {
	values := make(map[string]string, len(point.Attributes))
	for _, attribute := range point.Attributes {
		values[attribute.Key] = attribute.Value.StringValue
	}
	return values
}

// dataPointCount is how many points one request carried, which is what the cap
// bounds.
func dataPointCount(export otlpExport) int {
	count := 0
	for _, resource := range export.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				count += len(pointsOf(metric))
			}
		}
	}
	return count
}

func TestEncodeBatchFollowsTheOTLPShape(t *testing.T) {
	body, err := encodeBatch([]Sample{
		{
			Name:   egressSeries,
			Labels: map[string]string{"resource_id": "0f6d"},
			Value:  4294967296,
			At:     pushInstant,
			Kind:   KindCounter,
		},
		{Name: seriesNovaTotalVMs, Value: 7, At: pushInstant, Kind: KindGauge},
	}, map[string]string{"platform": platformOpenStack, "cloud": testCloud})
	if err != nil {
		t.Fatalf("encoding the batch: %v", err)
	}

	metrics := metricsOf(t, decodeExport(t, body))
	if len(metrics) != 2 {
		t.Fatalf("the document carries %d metrics, want one per series name", len(metrics))
	}

	traffic := metrics[0]
	if traffic.Name != egressSeries || !strings.HasSuffix(traffic.Name, "_total") {
		t.Errorf("the counter is named %q, want %s, which ends in _total", traffic.Name, egressSeries)
	}
	if traffic.Sum == nil || traffic.Gauge != nil {
		t.Fatalf("%s is stated with sum %v and gauge %v, want a sum alone",
			traffic.Name, traffic.Sum, traffic.Gauge)
	}
	if traffic.Sum.AggregationTemporality != otlpCumulative || !traffic.Sum.IsMonotonic {
		t.Errorf("%s carries the temporality %d and isMonotonic %t, want %d and true",
			traffic.Name, traffic.Sum.AggregationTemporality, traffic.Sum.IsMonotonic, otlpCumulative)
	}
	point := onePointOf(t, traffic)
	if point.AsInt != "4294967296" {
		t.Errorf("%s carries the value %q, want the decimal string 4294967296", traffic.Name, point.AsInt)
	}
	if want := strconv.FormatInt(pushInstant.UnixNano(), 10); point.TimeUnixNano != want {
		t.Errorf("%s carries the instant %q, want %q", traffic.Name, point.TimeUnixNano, want)
	}

	inventory := metrics[1]
	if inventory.Gauge == nil || inventory.Sum != nil {
		t.Fatalf("%s is stated with gauge %v and sum %v, want a gauge alone",
			inventory.Name, inventory.Gauge, inventory.Sum)
	}
	if value := onePointOf(t, inventory).AsInt; value != "7" {
		t.Errorf("%s carries the value %q, want 7", inventory.Name, value)
	}

	// A metric that declared a unit is stored under a name with the unit
	// appended to it, which is not the name the dashboards read.
	if strings.Contains(string(body), `"unit"`) {
		t.Errorf("the document %s carries a unit member, want none", body)
	}
}

func TestEncodeBatchRefusesOneNameUnderTwoKinds(t *testing.T) {
	_, err := encodeBatch([]Sample{
		{Name: egressSeries, Value: 1, At: pushInstant, Kind: KindCounter},
		{Name: egressSeries, Value: 2, At: pushInstant, Kind: KindGauge},
	}, nil)
	if err == nil {
		t.Fatalf("encoding %s as a counter and as a gauge returned no error", egressSeries)
	}
	if !strings.Contains(err.Error(), egressSeries) {
		t.Errorf("the error %v does not name the metric %s", err, egressSeries)
	}
}

func TestEncodeBatchStatesAnEmptyBatchAsNoMetric(t *testing.T) {
	body, err := encodeBatch(nil, map[string]string{"cloud": testCloud})
	if err != nil {
		t.Fatalf("encoding an empty batch: %v", err)
	}
	if metrics := metricsOf(t, decodeExport(t, body)); len(metrics) != 0 {
		t.Errorf("an empty batch encodes to %s, want a document with no metric", body)
	}
}

func TestEncodeBatchLetsTheSampleWinALabelClash(t *testing.T) {
	body, err := encodeBatch([]Sample{{
		Name:   egressSeries,
		Labels: map[string]string{"cloud": "os-of-the-month"},
		At:     pushInstant,
		Kind:   KindCounter,
	}}, map[string]string{"platform": platformOpenStack, "cloud": "os-of-the-pusher"})
	if err != nil {
		t.Fatalf("encoding the batch: %v", err)
	}

	metrics := metricsOf(t, decodeExport(t, body))
	if len(metrics) != 1 {
		t.Fatalf("the document carries %d metrics, want 1", len(metrics))
	}
	if cloud := attributeValues(onePointOf(t, metrics[0]))["cloud"]; cloud != "os-of-the-month" {
		t.Errorf("the point carries the cloud %q, want the one its sample states", cloud)
	}
}

func TestPushCarriesTheLabelsEachFaceNeeds(t *testing.T) {
	server := newOTLPServer(t)
	err := pusherTo(server, 0).Push(t.Context(), []Sample{
		// The five labels TrafficOf places, which the concept makes mandatory for
		// every provider.
		{
			Name: egressSeries,
			Labels: map[string]string{
				"platform":      platformOpenStack,
				"cloud":         testCloud,
				"resource_type": typeInstance,
				"resource_id":   "0f6d",
				"project_id":    cloudTenant,
			},
			Value: 4096,
			At:    pushInstant,
			Kind:  KindCounter,
		},
		// An inventory sample carries the exporter's own labels and neither a
		// platform nor a cloud, because a scrape takes those from its job.
		{
			Name:   seriesNovaServerStatus,
			Labels: map[string]string{"id": "0f6d", "tenant_id": cloudTenant},
			Value:  1,
			At:     pushInstant,
			Kind:   KindGauge,
		},
		// An aggregate carries no labels of its own at all.
		{Name: seriesNovaTotalVMs, Value: 3, At: pushInstant, Kind: KindGauge},
	})
	if err != nil {
		t.Fatalf("pushing the three faces: %v", err)
	}

	seen := server.seen()
	if len(seen) != 1 {
		t.Fatalf("the endpoint saw %d requests, want 1", len(seen))
	}
	metrics := metricsOf(t, seen[0].export)
	if len(metrics) != 3 {
		t.Fatalf("the request carries %d metrics, want one per series name", len(metrics))
	}

	for _, tc := range []struct {
		metric otlpMetric
		want   []string
	}{
		{metrics[0], []string{"cloud", "platform", "project_id", "resource_id", "resource_type"}},
		{metrics[1], []string{"cloud", "id", "platform", "tenant_id"}},
		{metrics[2], []string{"cloud", "platform"}},
	} {
		if keys := attributeKeys(onePointOf(t, tc.metric)); !slices.Equal(keys, tc.want) {
			t.Errorf("%s carries the attributes %v, want %v, sorted by key", tc.metric.Name, keys, tc.want)
		}
	}

	values := attributeValues(onePointOf(t, metrics[1]))
	if values["platform"] != platformOpenStack || values["cloud"] != testCloud {
		t.Errorf("the inventory point is pushed under the platform %q and the cloud %q, want %s and %s",
			values["platform"], values["cloud"], platformOpenStack, testCloud)
	}
}

func TestPushSendsTheBasicCredential(t *testing.T) {
	server := newOTLPServer(t)
	if err := pusherTo(server, 0).Push(t.Context(), countSample()); err != nil {
		t.Fatalf("pushing one sample: %v", err)
	}

	seen := server.seen()
	if len(seen) != 1 {
		t.Fatalf("the endpoint saw %d requests, want 1", len(seen))
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte(pushUser+":"+pushPassword)); seen[0].authorization != want {
		t.Errorf("the endpoint saw the authorization %q, want the basic credential of %s",
			seen[0].authorization, pushUser)
	}
	if seen[0].contentType != "application/json" {
		t.Errorf("the endpoint saw the content type %q, want application/json", seen[0].contentType)
	}
}

func TestPushReportsARefusal(t *testing.T) {
	server := newOTLPServer(t, http.StatusUnauthorized)
	err := pusherTo(server, 2).Push(t.Context(), countSample())
	if err == nil {
		t.Fatalf("pushing to an endpoint that answers 401 returned no error")
	}
	for _, want := range []string{"pushing metrics to " + server.URL, "401"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %v does not state %q", err, want)
		}
	}
	if strings.Contains(err.Error(), pushPassword) {
		t.Errorf("the error states the password, want a message an operator can paste anywhere")
	}
	if n := len(server.seen()); n != 1 {
		t.Errorf("the endpoint saw %d requests, want 1: a 4xx is not retried", n)
	}
}

// TestPushRedactsACredentialInTheURL is the endpoint whose credential sits in
// the URL itself. ValidateMetrics refuses all three shapes, so a run never
// builds such a pusher, but a Pusher is built straight from a URL and the
// failure it formats is logged as JSON and shipped from there: the endpoint the
// message names carries neither half of a userinfo nor a parameter.
//
// Every shape is held against both faces of a failed push. One is a status the
// endpoint answered with. The other is a transport that never reached it, which
// http.Client reports inside a wrapper carrying the request's own URL, of which
// it masks the password half of a userinfo alone.
func TestPushRedactsACredentialInTheURL(t *testing.T) {
	const embedded = "s3cret-in-the-url"

	for _, endpoint := range []struct {
		name string
		of   func(*testing.T) string
	}{
		{
			name: "refusing the batch",
			of:   func(t *testing.T) string { return newOTLPServer(t, http.StatusUnauthorized).URL },
		},
		{
			name: "refusing the connection",
			of:   closedEndpoint,
		},
	} {
		for _, tc := range []struct {
			name  string
			carry func(*url.URL)
		}{
			{
				name:  "a password in the userinfo",
				carry: func(u *url.URL) { u.User = url.UserPassword(pushUser, embedded) },
			},
			{
				name:  "a token as the user",
				carry: func(u *url.URL) { u.User = url.User(embedded) },
			},
			{
				name:  "an api key in the query",
				carry: func(u *url.URL) { u.RawQuery = url.Values{"api-key": {embedded}}.Encode() },
			},
		} {
			t.Run(endpoint.name+" with "+tc.name, func(t *testing.T) {
				raw := endpoint.of(t)
				parsed, err := url.Parse(raw)
				if err != nil {
					t.Fatalf("parsing the stand-in endpoint %s: %v", raw, err)
				}
				tc.carry(parsed)

				err = NewPusher(parsed.String(), pushUser, pushPassword, testCloud, pushClient(0)).
					Push(t.Context(), countSample())
				if err == nil {
					t.Fatal("pushing to an endpoint that refuses the push returned no error")
				}
				if strings.Contains(err.Error(), embedded) {
					t.Errorf("the error %v states the credential the URL carried, want it redacted", err)
				}
				if !strings.Contains(err.Error(), parsed.Host) {
					t.Errorf("the error %v names no endpoint, want the host an operator mistyped", err)
				}
			})
		}
	}
}

func TestPushRetriesA429(t *testing.T) {
	server := newOTLPServer(t, http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusOK)
	if err := pusherTo(server, 2).Push(t.Context(), countSample()); err != nil {
		t.Fatalf("pushing through two rate limits: %v", err)
	}
	if n := len(server.seen()); n != 3 {
		t.Errorf("the endpoint saw %d requests, want the two refused ones and the accepted one", n)
	}
}

func TestPushGivesUpOnA503(t *testing.T) {
	server := newOTLPServer(t, http.StatusServiceUnavailable)
	err := pusherTo(server, 2).Push(t.Context(), countSample())
	if err == nil {
		t.Fatalf("pushing to an endpoint that stays down returned no error")
	}
	for _, want := range []string{"pushing metrics to " + server.URL, "503"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %v does not state %q", err, want)
		}
	}
	if n := len(server.seen()); n != 3 {
		t.Errorf("the endpoint saw %d requests, want the attempt and its two retries", n)
	}
}

func TestPushSendsNothingForAnEmptyBatch(t *testing.T) {
	server := newOTLPServer(t)
	pusher := pusherTo(server, 0)
	if err := pusher.Push(t.Context(), nil); err != nil {
		t.Errorf("pushing no samples at all: %v", err)
	}
	if err := pusher.Push(t.Context(), []Sample{}); err != nil {
		t.Errorf("pushing an empty batch: %v", err)
	}
	if n := len(server.seen()); n != 0 {
		t.Errorf("the endpoint saw %d requests, want none for a batch with no point", n)
	}
}

func TestPushSplitsAtTheCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		want  []int
	}{
		{name: "one point past the cap", count: maxDataPoints + 1, want: []int{maxDataPoints, 1}},
		{name: "exactly the cap", count: maxDataPoints, want: []int{maxDataPoints}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := make([]Sample, 0, tc.count)
			for i := range tc.count {
				samples = append(samples, Sample{
					Name: seriesNovaTotalVMs, Value: int64(i), At: pushInstant, Kind: KindGauge,
				})
			}

			server := newOTLPServer(t)
			if err := pusherTo(server, 0).Push(t.Context(), samples); err != nil {
				t.Fatalf("pushing %d samples: %v", tc.count, err)
			}

			counts := make([]int, 0, len(tc.want))
			for _, request := range server.seen() {
				counts = append(counts, dataPointCount(request.export))
			}
			if !slices.Equal(counts, tc.want) {
				t.Errorf("%d samples were pushed as the requests %v, want %v", tc.count, counts, tc.want)
			}
		})
	}
}

func TestPushStopsOnACancelledContext(t *testing.T) {
	server := newOTLPServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := pusherTo(server, 0).Push(ctx, countSample())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("pushing under a cancelled context returned %v, want a context.Canceled a loop reads as its stop", err)
	}
	if n := len(server.seen()); n != 0 {
		t.Errorf("the endpoint saw %d requests, want none from a run that was already stopped", n)
	}
}
