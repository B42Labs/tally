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

	"github.com/b42labs/tally/internal/refdoc"
)

// openAPIDocument is the contract the two Reporting API pages are rendered
// from, and problemSource holds the problem types its errors carry. Both are
// reached from this directory, which is where the test runs.
const (
	openAPIDocument = "../api/reporting/openapi.yaml"
	problemSource   = "../internal/reporting/httpapi/problem/problem.go"
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
