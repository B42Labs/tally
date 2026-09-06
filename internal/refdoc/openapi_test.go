package refdoc

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// realContract is the contract of the Reporting API, which is the document the
// three renderers are written for.
const realContract = "../../api/reporting/openapi.yaml"

// The size of that contract, asserted so that an operation or a schema added
// without a section on the page is noticed here rather than by a reader.
const (
	realOperations = 26
	realComponents = 38
)

// loadDocument reads a contract the way the caller of these renderers does:
// through the loader, which resolves every reference so that a section can be
// rendered from the value while the reference itself still names the schema.
func loadDocument(t *testing.T, path string) *openapi3.T {
	t.Helper()

	doc, err := openapi3.NewLoader().LoadFromFile(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	return doc
}

// countHeadings reports how many lines open a heading of that level.
func countHeadings(text, prefix string) int {
	count := 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

func TestOpenAPISecurity(t *testing.T) {
	got, err := OpenAPISecurity(loadDocument(t, filepath.Join("testdata", "openapi.yaml")))
	if err != nil {
		t.Fatalf("OpenAPISecurity() error = %v, want nil", err)
	}

	assertWant(t, "security.want.md", got)
}

func TestOpenAPISecurityRejectsANilDocument(t *testing.T) {
	_, err := OpenAPISecurity(nil)
	if err == nil {
		t.Fatal("OpenAPISecurity(nil) error = nil, want an error")
	}
	if want := "refdoc: nil document"; err.Error() != want {
		t.Errorf("OpenAPISecurity(nil) error = %q, want %q", err, want)
	}
}

func TestOpenAPIOperations(t *testing.T) {
	got, err := OpenAPIOperations(loadDocument(t, filepath.Join("testdata", "openapi.yaml")))
	if err != nil {
		t.Fatalf("OpenAPIOperations() error = %v, want nil", err)
	}

	assertWant(t, "operations.want.md", got)
}

func TestOpenAPIOperationsRendersEachOperationShape(t *testing.T) {
	got, err := OpenAPIOperations(loadDocument(t, filepath.Join("testdata", "openapi.yaml")))
	if err != nil {
		t.Fatalf("OpenAPIOperations() error = %v, want nil", err)
	}

	for _, want := range []string{
		// An operation the contract leaves unsecured says so rather than
		// leaving the line out.
		"No credential.",
		"Security: `apiToken`",
		// A referenced parameter is rendered from the component it names.
		"| `cursor` | `query` | no | string | The `next_cursor` of the page",
		// An enum is its values, and a format is appended to the type.
		"| `kind` | `query` | no | `draft`, `final` |",
		"| `since` | `query` | no | string, `date-time` |",
		// A body of two alternatives names both.
		"The request body is `application/json`, a [Item](/reference/api/reporting-api-schemas#item) " +
			"or an array of [Item](/reference/api/reporting-api-schemas#item).",
		// The shared problem response and one declared with the media type
		// itself are both the problem document.
		"| `500` | The request failed. The body says how. | `application/problem+json` |",
		"| `413` | The batch carried more items than one call takes. | `application/problem+json` |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}

	// The methods of one path follow the order a reader walks them, whatever
	// order the document spells them in.
	assertOrder(t, got, []string{
		"### `GET /healthz`",
		"### `GET /api/v1/items/{cloud}`",
		"### `POST /api/v1/items/{cloud}`",
	})
}

func TestOpenAPIOperationsRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		doc  *openapi3.T
		want string
	}{
		"nil document":   {doc: nil, want: "refdoc: nil document"},
		"no paths block": {doc: &openapi3.T{}, want: "refdoc: the document declares no paths"},
		"empty paths": {
			doc:  &openapi3.T{Paths: openapi3.NewPaths()},
			want: "refdoc: the document declares no paths",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := OpenAPIOperations(tc.doc)
			if err == nil {
				t.Fatalf("OpenAPIOperations() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("OpenAPIOperations() error = %q, want %q", err, tc.want)
			}
		})
	}
}

func TestOpenAPISchemas(t *testing.T) {
	got, err := OpenAPISchemas(loadDocument(t, filepath.Join("testdata", "openapi.yaml")))
	if err != nil {
		t.Fatalf("OpenAPISchemas() error = %v, want nil", err)
	}

	assertWant(t, "schemas.want.md", got)
}

func TestOpenAPISchemasRendersEachMemberShape(t *testing.T) {
	got, err := OpenAPISchemas(loadDocument(t, filepath.Join("testdata", "openapi.yaml")))
	if err != nil {
		t.Fatalf("OpenAPISchemas() error = %v, want nil", err)
	}

	for _, want := range []string{
		// A member the contract constrains on purpose with nothing reads any.
		"| `payload` | any | no |",
		"| `tags` | array of string | no |",
		"| `items` | array of [Item](#item) | yes |",
		"| `id` | [Uuid](#uuid) | yes |",
		"| `note` | string or null | no |",
		// A schema without members is one sentence carrying its format and its
		// pattern.
		"A string, format `uuid`, matching `^[0-9a-fA-F]{8}-",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestOpenAPISchemasRejectsANilDocument(t *testing.T) {
	_, err := OpenAPISchemas(nil)
	if err == nil {
		t.Fatal("OpenAPISchemas(nil) error = nil, want an error")
	}
	if want := "refdoc: nil document"; err.Error() != want {
		t.Errorf("OpenAPISchemas(nil) error = %q, want %q", err, want)
	}
}

func TestOpenAPIRendersTheContractOfThisRepository(t *testing.T) {
	doc := loadDocument(t, realContract)

	operations, err := OpenAPIOperations(doc)
	if err != nil {
		t.Fatalf("OpenAPIOperations() error = %v, want nil", err)
	}
	if n := countHeadings(operations, "### "); n != realOperations {
		t.Errorf("the contract rendered %d operations, want %d", n, realOperations)
	}
	for _, want := range []string{
		"### `GET /healthz`\n\nLiveness probe",
		"Security: `ingestToken`",
		"### `POST /internal/sync/{cloud}`",
		"### `GET /api/v1/resources/{cloud}/{resource_type}/{resource_id}/events`",
	} {
		if !strings.Contains(operations, want) {
			t.Errorf("the rendering does not carry %q", want)
		}
	}
	// The health probes take no credential, and the ingest path answers a
	// batch past its bound with 413.
	if !strings.Contains(healthzSection(t, operations), "No credential.") {
		t.Error("the liveness probe does not read as unsecured")
	}
	if !strings.Contains(ingestSection(t, operations), "| `413` |") {
		t.Error("the ingest operation carries no 413 row")
	}

	schemas, err := OpenAPISchemas(doc)
	if err != nil {
		t.Fatalf("OpenAPISchemas() error = %v, want nil", err)
	}
	if n := countHeadings(schemas, "### "); n != realComponents {
		t.Errorf("the contract rendered %d schemas, want %d", n, realComponents)
	}

	security, err := OpenAPISecurity(doc)
	if err != nil {
		t.Fatalf("OpenAPISecurity() error = %v, want nil", err)
	}
	if !strings.Contains(security, "| `apiToken` | `http bearer` |") {
		t.Errorf("the security table does not carry the API token:\n%s", security)
	}
}

// healthzSection is the section of the liveness probe.
func healthzSection(t *testing.T, rendered string) string {
	t.Helper()

	return section(t, rendered, "### `GET /healthz`")
}

// ingestSection is the section of the ingest operation.
func ingestSection(t *testing.T, rendered string) string {
	t.Helper()

	return section(t, rendered, "### `POST /api/v1/events`")
}

// section is the text from one heading to the next, so that a claim about one
// operation is not answered by another one's rows.
func section(t *testing.T, rendered, heading string) string {
	t.Helper()

	begin := strings.Index(rendered, heading)
	if begin < 0 {
		t.Fatalf("the rendering carries no %s", heading)
	}
	rest := rendered[begin+len(heading):]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		return rest[:end]
	}
	return rest
}
