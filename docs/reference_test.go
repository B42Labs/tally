// This file pins the generated blocks of the reference pages to the sources
// they are rendered from. A reference page states a contract, and a contract
// the code stopped keeping is worse than no page at all: nothing about a stale
// operation table looks stale. Each subtest renders the blocks of one page and
// hands them to refdoc.Verify, so a route added to the OpenAPI document or a
// constant renamed in the Go source fails `go test ./docs/` until the page
// carries the change. The pages are rewritten rather than reported when
// TALLY_UPDATE_DOCS=1 is set, which is how a block is refreshed after the
// source moved; the OpenAPI document itself is what `make generate` builds the
// server from, so the page, the server and the document say one thing. The
// hand-written text outside the markers is not read here: docs_test.go judges
// it against the authoring conventions.
package docs_test

import (
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/prometheus/client_golang/prometheus"

	engineconfig "github.com/b42labs/tally/internal/engine/config"
	"github.com/b42labs/tally/internal/providers/openstack"
	"github.com/b42labs/tally/internal/providers/openstack/simulator"
	"github.com/b42labs/tally/internal/refdoc"
	reportingconfig "github.com/b42labs/tally/internal/reporting/config"
	reportingmetrics "github.com/b42labs/tally/internal/reporting/metrics"
)

// openAPIDocument is the contract the two Reporting API pages are rendered
// from, and problemSource holds the problem types its errors carry. Both are
// reached from this directory, which is where the test runs.
const (
	openAPIDocument = "../api/reporting/openapi.yaml"
	problemSource   = "../internal/reporting/httpapi/problem/problem.go"
)

// The sources the two command line pages render their document types from. A
// simulator page states three of them, because the control endpoint, the stream
// file and the oracle are written by three files of one package.
const (
	simulatorControlSource = "../internal/providers/openstack/simulator/control.go"
	simulatorStreamSource  = "../internal/providers/openstack/simulator/stream.go"
	simulatorOracleSource  = "../internal/providers/openstack/simulator/oracle.go"
	sliceDocumentSource    = "../cmd/tally-vertical-slice/compute.go"
)

// The configuration structs the four settings pages render their tables from.
// A settings table is rendered against the package's EnvNames as well, which is
// where the *_FILE companion of a secret is looked up.
const (
	reportingConfigSource = "../internal/reporting/config/config.go"
	engineConfigSource    = "../internal/engine/config/config.go"
	collectorConfigSource = "../internal/providers/openstack/config.go"
	simulatorConfigSource = "../internal/providers/openstack/simulator/config.go"
)

// The two configuration file formats: the source the entry of each one is
// rendered from, and the file its example is fenced from. The examples are the
// ones the repository ships, so a page shows a document that is deployed rather
// than one written for the page.
const (
	cloudsSource          = "../internal/reporting/reconciliation/config.go"
	cloudsExample         = "../deploy/kubernetes/overlays/dev/reconciliation/clouds-config.yaml"
	counterSourcesSource  = "../internal/engine/counters/counters.go"
	counterSourcesExample = "../cmd/tally-engine/counter-sources.example.yaml"
)

// The sources the schema and format pages render from: the canonical event, the
// two embedded JSON Schemas, the pricing model the repository ships, and the
// mapping literal the OpenStack collector maps notifications with.
const (
	eventSource             = "../internal/core/event/event.go"
	pricingSchemaSource     = "../internal/engine/pricing/pricing.schema.json"
	pricingExample          = "../pricing/2026-03.yaml"
	adjustmentsSchemaSource = "../internal/core/adjustment/adjustments_schema.json"
	adjustmentsSource       = "../internal/engine/adjustments/adjustments.go"
	mappingSource           = "../internal/providers/openstack/mapping.go"
)

// The manifests the three observability pages render from: the scrape
// configuration the store reads, the rules the evaluator loads, the routing a
// fired alert takes, and the directory the provisioned dashboards are shipped
// out of. Each is the file a cluster runs, so a page states what is deployed
// rather than what was once written down.
const (
	scrapeSource     = "../deploy/kubernetes/base/victoriametrics/scrape.yaml"
	rulesSource      = "../deploy/kubernetes/base/vmalert/rules.yaml"
	routingSource    = "../deploy/kubernetes/base/alertmanager/config.yaml"
	dashboardsSource = "../deploy/kubernetes/base/grafana/dashboards"
)

// The sources the export page renders its documents from: the two file writers
// that declare them and the two packages whose types the writers re-render.
// goldenDir holds the examples, which are the files the export tests compare a
// written directory against, so the page shows output a test produced rather
// than output written for the page.
const (
	exportJSONSource      = "../internal/engine/export/json.go"
	exportKickbacksSource = "../internal/engine/export/kickbacks.go"
	exportRollupSource    = "../internal/engine/export/rollup.go"
	statementsSource      = "../internal/engine/statements/statements.go"
	correctionsSource     = "../internal/engine/corrections/corrections.go"
	goldenDir             = "../internal/engine/export/testdata/golden"
)

// problemTypes are the constants the errors table of the endpoints page lists,
// in the order the source declares them.
var problemTypes = []string{
	"TypeValidation",
	"TypeUnauthorized",
	"TypeForbidden",
	"TypeNotFound",
	"TypeMethodNotAllowed",
	"TypeConflict",
	"TypePayloadTooLarge",
	"TypeHistoryTooLong",
	"TypeResultTooLarge",
	"TypeNotImplemented",
	"TypeRelationCycle",
	"TypeInternal",
	"TypeUnavailable",
}

func TestReferencePagesAreCurrent(t *testing.T) {
	t.Run("api/reporting-api.md", func(t *testing.T) {
		document := loadOpenAPI(t)
		security, securityErr := refdoc.OpenAPISecurity(document)
		operations, operationsErr := refdoc.OpenAPIOperations(document)
		types, typesErr := refdoc.Consts(readSource(t, problemSource), problemTypes...)

		refdoc.Verify(t, referencePage("api/reporting-api.md"), map[string]string{
			"security":      render(t, security, securityErr),
			"problem-types": render(t, types, typesErr),
			"operations":    render(t, operations, operationsErr),
		})
	})

	t.Run("api/reporting-api-schemas.md", func(t *testing.T) {
		schemas, err := refdoc.OpenAPISchemas(loadOpenAPI(t))

		refdoc.Verify(t, referencePage("api/reporting-api-schemas.md"), map[string]string{
			"schemas": render(t, schemas, err),
		})
	})

	t.Run("command-line/tally-openstack-simulator.md", func(t *testing.T) {
		clock, clockErr := refdoc.Struct(readSource(t, simulatorControlSource), "json", "clockDocument")
		line, lineErr := refdoc.Struct(readSource(t, simulatorStreamSource), "json", "Line")
		oracleSource := readSource(t, simulatorOracleSource)
		oracle, oracleErr := refdoc.Struct(oracleSource, "json",
			"Oracle", "OracleResource", "OracleInterval", "OracleCount", "OracleTraffic")
		format, formatErr := refdoc.Consts(oracleSource, "oracleFormat")

		refdoc.Verify(t, referencePage("command-line/tally-openstack-simulator.md"), map[string]string{
			"clock-document": render(t, clock, clockErr),
			"stream-line":    render(t, line, lineErr),
			"oracle":         render(t, oracle, oracleErr),
			"oracle-format":  render(t, format, formatErr),
		})
	})

	t.Run("command-line/tally-vertical-slice.md", func(t *testing.T) {
		document, err := refdoc.Struct(readSource(t, sliceDocumentSource), "json",
			"document", "periodDoc", "resourceDoc", "recordDoc", "dimensionDoc")

		refdoc.Verify(t, referencePage("command-line/tally-vertical-slice.md"), map[string]string{
			"document": render(t, document, err),
		})
	})

	t.Run("configuration/tally-reporting.md", func(t *testing.T) {
		settings, err := refdoc.Settings(readSource(t, reportingConfigSource), "Config",
			reportingconfig.EnvNames)

		refdoc.Verify(t, referencePage("configuration/tally-reporting.md"), map[string]string{
			"settings": render(t, settings, err),
		})
	})

	t.Run("configuration/tally-engine.md", func(t *testing.T) {
		settings, err := refdoc.Settings(readSource(t, engineConfigSource), "Config",
			engineconfig.EnvNames)

		refdoc.Verify(t, referencePage("configuration/tally-engine.md"), map[string]string{
			"settings": render(t, settings, err),
		})
	})

	t.Run("configuration/tally-openstack-collector.md", func(t *testing.T) {
		settings, err := refdoc.Settings(readSource(t, collectorConfigSource), "Config",
			openstack.EnvNames)

		refdoc.Verify(t, referencePage("configuration/tally-openstack-collector.md"), map[string]string{
			"settings": render(t, settings, err),
		})
	})

	t.Run("configuration/tally-openstack-simulator.md", func(t *testing.T) {
		settings, err := refdoc.Settings(readSource(t, simulatorConfigSource), "Config",
			simulator.EnvNames)

		refdoc.Verify(t, referencePage("configuration/tally-openstack-simulator.md"), map[string]string{
			"settings": render(t, settings, err),
		})
	})

	t.Run("configuration/clouds-file.md", func(t *testing.T) {
		entry, err := refdoc.Struct(readSource(t, cloudsSource), "yaml", "CloudConfig")

		refdoc.Verify(t, referencePage("configuration/clouds-file.md"), map[string]string{
			"entry":   render(t, entry, err),
			"example": refdoc.Fenced("yaml", readSource(t, cloudsExample)),
		})
	})

	t.Run("configuration/counter-sources-file.md", func(t *testing.T) {
		entry, err := refdoc.Struct(readSource(t, counterSourcesSource), "yaml", "sourceFile")

		refdoc.Verify(t, referencePage("configuration/counter-sources-file.md"), map[string]string{
			"entry":   render(t, entry, err),
			"example": refdoc.Fenced("yaml", readSource(t, counterSourcesExample)),
		})
	})

	t.Run("formats/canonical-event.md", func(t *testing.T) {
		source := readSource(t, eventSource)
		wire, wireErr := refdoc.Struct(source, "json", "Event", "PayloadEnvelope")
		bounds, boundsErr := refdoc.Consts(source,
			"eventIDMaxLen", "eventTypeMaxLen", "identifierMaxLen", "stateMaxLen")
		origins, originsErr := refdoc.Consts(source,
			"SourceCollector", "SourceReconciliation",
			"CategoryCreate", "CategoryUpdate", "CategoryDelete")

		refdoc.Verify(t, referencePage("formats/canonical-event.md"), map[string]string{
			"event":   render(t, wire, wireErr),
			"bounds":  render(t, bounds, boundsErr),
			"sources": render(t, origins, originsErr),
		})
	})

	t.Run("formats/pricing-model.md", func(t *testing.T) {
		schema, err := refdoc.JSONSchema(readSource(t, pricingSchemaSource))

		refdoc.Verify(t, referencePage("formats/pricing-model.md"), map[string]string{
			"schema":  render(t, schema, err),
			"example": refdoc.Fenced("yaml", readSource(t, pricingExample)),
		})
	})

	t.Run("formats/pricing-adjustments.md", func(t *testing.T) {
		schema, schemaErr := refdoc.JSONSchema(readSource(t, adjustmentsSchemaSource))
		line, lineErr := refdoc.Struct(readSource(t, adjustmentsSource), "json", "Line")

		refdoc.Verify(t, referencePage("formats/pricing-adjustments.md"), map[string]string{
			"schema": render(t, schema, schemaErr),
			"line":   render(t, line, lineErr),
		})
	})

	t.Run("formats/exports.md", func(t *testing.T) {
		index, indexErr := refdoc.Struct(readSource(t, exportJSONSource), "json",
			"runDocument", "statementEntry", "rollupIndex", "rollupEntry")
		statement, statementErr := refdoc.Struct(readSource(t, statementsSource), "json",
			"Document", "BillingPeriod", "LineItem", "Period", "RelatedCost")
		creditNote, creditNoteErr := refdoc.Struct(readSource(t, correctionsSource), "json",
			"CreditNote", "LineItem", "Change", "AdjustmentChange", "RelatedCost")
		settlement, settlementErr := refdoc.Struct(readSource(t, exportKickbacksSource), "json",
			"kickbacksDocument", "beneficiaryEntry", "kickbackEntry")
		rollup, rollupErr := refdoc.Struct(readSource(t, exportRollupSource), "json",
			"rollupDocument", "rollupMemberEntry")
		// The golden files the examples are fenced from, read the way every other
		// source of this test is: relative to this directory.
		golden := func(name string) []byte { return readSource(t, goldenDir+"/"+name) }

		refdoc.Verify(t, referencePage("formats/exports.md"), map[string]string{
			"run-json":    render(t, index, indexErr),
			"statement":   render(t, statement, statementErr),
			"credit-note": render(t, creditNote, creditNoteErr),
			"kickbacks":   render(t, settlement, settlementErr),
			"rollup":      render(t, rollup, rollupErr),

			"example-run-json":            refdoc.Fenced("json", golden("regular/run.json")),
			"example-statement":           refdoc.Fenced("json", golden("regular/statement-os-prod%2Fproj-456.json")),
			"example-correction-run-json": refdoc.Fenced("json", golden("correction/run.json")),
			"example-credit-note":         refdoc.Fenced("json", golden("correction/credit-note-os-prod%2Fproj-456.json")),
			"example-kickbacks":           refdoc.Fenced("json", golden("kickbacks/regular.json")),
			"example-kickback-deltas":     refdoc.Fenced("json", golden("kickbacks/correction.json")),
			"example-rollup-run-json":     refdoc.Fenced("json", golden("rollup/run.json")),
			"example-rollup":              refdoc.Fenced("json", golden("rollup/rollup-meta%2Fcustomer-alpha.json")),

			"example-rated-csv":     refdoc.Fenced("csv", golden("regular/rated.csv")),
			"example-deltas-csv":    refdoc.Fenced("csv", golden("correction/deltas.csv")),
			"example-kickbacks-csv": refdoc.Fenced("csv", golden("kickbacks/regular.csv")),
			"example-rollup-csv":    refdoc.Fenced("csv", golden("rollup/rollup.csv")),
		})
	})

	t.Run("formats/notification-mapping.md", func(t *testing.T) {
		mapping, err := refdoc.MappingTable(readSource(t, mappingSource))

		refdoc.Verify(t, referencePage("formats/notification-mapping.md"), map[string]string{
			"mapping": render(t, mapping, err),
		})
	})

	t.Run("observability/metrics.md", func(t *testing.T) {
		reportingAPI, reportingErr := refdoc.Metrics(recordedReportingRegistry())
		collector, collectorErr := refdoc.Metrics(recordedCollectorRegistry())
		jobs, jobsErr := refdoc.ScrapeJobs(readSource(t, scrapeSource))

		refdoc.Verify(t, referencePage("observability/metrics.md"), map[string]string{
			"reporting-api": render(t, reportingAPI, reportingErr),
			"collector":     render(t, collector, collectorErr),
			"scrape-jobs":   render(t, jobs, jobsErr),
		})
	})

	t.Run("observability/alert-rules.md", func(t *testing.T) {
		rules, rulesErr := refdoc.AlertRules(readSource(t, rulesSource))
		routing, routingErr := refdoc.AlertRouting(readSource(t, routingSource))

		refdoc.Verify(t, referencePage("observability/alert-rules.md"), map[string]string{
			"rules":   render(t, rules, rulesErr),
			"routing": render(t, routing, routingErr),
		})
	})

	t.Run("observability/dashboards.md", func(t *testing.T) {
		dashboards, err := refdoc.Dashboards(readDashboards(t))

		refdoc.Verify(t, referencePage("observability/dashboards.md"), map[string]string{
			"dashboards": render(t, dashboards, err),
		})
	})
}

// recordedReportingRegistry is the Reporting API's registry with one sample on
// every instrument a recording method writes. A vector without a child is
// gathered as nothing, so the exposition would state the type of none of them
// and the rendering would fall back to the name throughout; recording once
// leaves the type of the eight counters read off the exposition, which is what
// makes a counter misnamed as a gauge fail here. tally_current_resources is
// written by the refresher rather than by a recording method and stays on the
// name.
func recordedReportingRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	m := reportingmetrics.New(reg)

	m.EventIngested("openstack", "os-prod-eu1", "instance", "compute.instance.create.end", "collector")
	m.EventDeduplicated("os-prod-eu1")
	m.EventRejected("os-prod-eu1", "schema: the item carries a NUL character")
	m.SizeUnvalidated("openstack", "instance")
	m.ProjectionReplayed("os-prod-eu1")
	m.SyncRunFinished("os-prod-eu1", "completed")
	m.ResourcesReconciled("os-prod-eu1", "created", 1)
	m.SyncErrorsRecorded("os-prod-eu1", 1)
	return reg
}

// recordedCollectorRegistry is the OpenStack collector's registry, recorded the
// same way. The two gauges are read at scrape time and take the functions the
// binary wires to the outbox; a page states what they report rather than what
// they hold, so the fixture reports an empty outbox.
func recordedCollectorRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	c := openstack.NewMetrics(reg, func() float64 { return 0 }, func() float64 { return 0 })

	c.Consumed("compute.instance.create.end")
	c.Skipped("compute.instance.reboot.end")
	c.Unparseable()
	c.Delivered(1)
	c.DeliveryError()
	return reg
}

// readDashboards reads the provisioned dashboard files, keyed by the file name
// the page names each dashboard by. The directory is read rather than listed,
// so a dashboard added to the ConfigMap without a section on the page fails the
// subtest instead of being published unlisted.
func readDashboards(t *testing.T) map[string][]byte {
	t.Helper()

	entries, err := os.ReadDir(dashboardsSource)
	if err != nil {
		t.Fatalf("%s: %v", dashboardsSource, err)
	}

	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		files[entry.Name()] = readSource(t, dashboardsSource+"/"+entry.Name())
	}
	return files
}

// referencePage is the page under docs/reference/ a subtest verifies. The path
// leads out of this directory and back into it, so a failure names the page the
// way the repository does.
func referencePage(name string) string {
	return "../docs/reference/" + name
}

// readSource reads a file a renderer takes as its input. A source that cannot
// be read is a broken checkout rather than a stale page, so it fails the
// subtest before any page is touched.
func readSource(t *testing.T, path string) []byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return raw
}

// render is the text a renderer produced. A renderer that failed leaves the
// page alone: rewriting one block of a page whose other block could not be
// rendered would put half an update on disk.
func render(t *testing.T, text string, err error) string {
	t.Helper()

	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	return text
}

// loadOpenAPI reads the Reporting API contract with the loader that resolves
// its internal references, which is the same loader the server validates
// requests with.
func loadOpenAPI(t *testing.T) *openapi3.T {
	t.Helper()

	document, err := openapi3.NewLoader().LoadFromFile(openAPIDocument)
	if err != nil {
		t.Fatalf("%s: %v", openAPIDocument, err)
	}
	return document
}
