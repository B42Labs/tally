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
		// An operation the contract opens up with an empty block says so rather
		// than leaving the line out, and one that declares no block at all
		// takes the document's credential.
		"No credential.",
		"Security: `apiToken`",
		// A requirement naming two schemes asks for both, and a second
		// requirement is an alternative to the first.
		"Security: `ingestToken`, or `apiToken` and `internalToken`",
		// A referenced parameter is rendered from the component it names.
		"| `cursor` | `query` | no | string | The `next_cursor` of the page",
		// An enum is its values, and a format is appended to the type.
		"| `kind` | `query` | no | `draft`, `final` |",
		"| `since` | `query` | no | string, `date-time` |",
		// A bound the server answers 400 past, and the value a caller who
		// omits the parameter sends, are on the row rather than nowhere.
		"| `limit` | `query` | no | integer, 1 to 1000, default `100` |",
		// A body of two alternatives names both.
		"The request body is `application/json`, an [Item](/reference/api/reporting-api-schemas#item) " +
			"or an array of [Item](/reference/api/reporting-api-schemas#item).",
		// A body that is not JSON is named by its media type, which is what
		// keeps it apart from a status carrying no body at all.
		"| `200` | The service is alive. | `text/plain` | none |",
		// The shared problem response and one declared with the media type
		// itself are both the problem document.
		"| `500` | The request failed. The body says how. | `application/problem+json` |",
		"| `413` | The batch carried more items than one call takes. | `application/problem+json` |",
		// The header a created resource is addressed by, beside the
		// correlation id every response of the contract declares.
		"| `201` | The items as they now stand. | " +
			"[ItemList](/reference/api/reporting-api-schemas#itemlist) | `Location` |",
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
		"### `PUT /api/v1/items/{cloud}`",
	})
}

func TestOpenAPIOperationsRendersThePathParametersFirst(t *testing.T) {
	got, err := OpenAPIOperations(loadDocument(t, filepath.Join("testdata", "openapi.yaml")))
	if err != nil {
		t.Fatalf("OpenAPIOperations() error = %v, want nil", err)
	}

	// The path template declares its parameter on the path item, so the row
	// stands above the rows of the parameters the operation declares itself.
	assertOrder(t, section(t, got, "### `GET /api/v1/items/{cloud}`"), []string{
		"| `cloud` | `path` | yes | string | The installation the items live in. |",
		"| `kind` | `query` |",
		"| `cursor` | `query` |",
	})

	// An operation declaring no parameter of its own still carries the ones of
	// the path it stands under, which is a table it would render none of.
	ingest := section(t, got, "### `POST /api/v1/items/{cloud}`")
	if !strings.Contains(ingest, "| `cloud` | `path` | yes | string |") {
		t.Errorf("the ingest operation carries no row for the path parameter:\n%s", ingest)
	}
}

// numberParameter is one query parameter of type number held to the bounds the
// caller sets, which is the shape a bound of either kind arrives in.
func numberParameter(name string, bounds *openapi3.Schema) *openapi3.ParameterRef {
	bounds.Type = &openapi3.Types{"number"}
	return &openapi3.ParameterRef{Value: &openapi3.Parameter{
		Name:   name,
		In:     "query",
		Schema: &openapi3.SchemaRef{Value: bounds},
	}}
}

// openBound is what OpenAPI 3.0 writes an open end as: a flag beside the
// minimum or the maximum it opens, rather than a bound of its own.
func openBound() openapi3.ExclusiveBound {
	return openapi3.ExclusiveBound{Bool: openapi3.BoolPtr(true)}
}

func TestOpenAPIOperationsWordsAnOpenBoundApart(t *testing.T) {
	// The open and the closed range carry the same minimum and maximum, so a
	// row that words them alike names an endpoint the server answers 400 to as
	// a value in range.
	paths := openapi3.NewPaths()
	paths.Set("/items", &openapi3.PathItem{Get: &openapi3.Operation{
		OperationID: "listItems",
		Parameters: openapi3.Parameters{
			numberParameter("closed", &openapi3.Schema{
				Min: openapi3.Float64Ptr(0),
				Max: openapi3.Float64Ptr(1),
			}),
			numberParameter("open_low", &openapi3.Schema{
				Min:          openapi3.Float64Ptr(0),
				Max:          openapi3.Float64Ptr(1),
				ExclusiveMin: openBound(),
			}),
			numberParameter("open_high", &openapi3.Schema{
				Min:          openapi3.Float64Ptr(0),
				Max:          openapi3.Float64Ptr(1),
				ExclusiveMax: openBound(),
			}),
			numberParameter("open_both", &openapi3.Schema{
				Min:          openapi3.Float64Ptr(0),
				Max:          openapi3.Float64Ptr(1),
				ExclusiveMin: openBound(),
				ExclusiveMax: openBound(),
			}),
			numberParameter("open_low_only", &openapi3.Schema{
				Min:          openapi3.Float64Ptr(0),
				ExclusiveMin: openBound(),
			}),
			numberParameter("open_high_only", &openapi3.Schema{
				Max:          openapi3.Float64Ptr(1),
				ExclusiveMax: openBound(),
			}),
		},
	}})

	got, err := OpenAPIOperations(&openapi3.T{Paths: paths})
	if err != nil {
		t.Fatalf("OpenAPIOperations() error = %v, want nil", err)
	}

	for _, want := range []string{
		"| `closed` | `query` | no | number, 0 to 1 |",
		"| `open_low` | `query` | no | number, above 0 and at most 1 |",
		"| `open_high` | `query` | no | number, at least 0 and below 1 |",
		"| `open_both` | `query` | no | number, above 0 and below 1 |",
		"| `open_low_only` | `query` | no | number, above 0 |",
		"| `open_high_only` | `query` | no | number, below 1 |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
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

func TestOpenAPIOperationsRejectsAnEmptySecurityRequirement(t *testing.T) {
	// An empty requirement is what OpenAPI writes optional security as, and
	// there is no honest word for a credential that is asked for and not.
	paths := openapi3.NewPaths()
	paths.Set("/items", &openapi3.PathItem{Get: &openapi3.Operation{
		OperationID: "listItems",
		Security:    &openapi3.SecurityRequirements{openapi3.SecurityRequirement{}},
	}})

	_, err := OpenAPIOperations(&openapi3.T{Paths: paths})
	if err == nil {
		t.Fatal("OpenAPIOperations() error = nil, want an error")
	}
	want := "refdoc: listItems declares an empty security requirement"
	if err.Error() != want {
		t.Errorf("OpenAPIOperations() error = %q, want %q", err, want)
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
		// The article in front of a body names the schema the way it is
		// spelled: a vowel takes an, every other letter takes a.
		"body is `application/json`, an [EventInput](",
		"body is `application/json`, a [SyncRequest](",
	} {
		if !strings.Contains(operations, want) {
			t.Errorf("the rendering does not carry %q", want)
		}
	}
	// The health probes take no credential, and the ingest path answers a
	// batch past its bound with 413. The probe answers the exposition format
	// rather than nothing, which is what a status without a body reads as.
	healthz := healthzSection(t, operations)
	if !strings.Contains(healthz, "No credential.") {
		t.Error("the liveness probe does not read as unsecured")
	}
	if !strings.Contains(healthz, "| `200` | The service is alive. | `text/plain` |") {
		t.Errorf("the liveness probe reads as answering no body:\n%s", healthz)
	}
	if !strings.Contains(ingestSection(t, operations), "| `413` |") {
		t.Error("the ingest operation carries no 413 row")
	}

	// The bounds a caller is answered 400 past and the value one who omits the
	// parameter sends are on the row: neither is anywhere else on the page.
	for _, want := range []string{
		"| `depth` | `query` | no | integer, 1 to 10, default `1` |",
		"| `limit` | `query` | no | integer, 1 to 1000, default `100` |",
		"| `direction` | `query` | no | `outgoing`, `incoming`, `both`, default `both` |",
	} {
		if !strings.Contains(operations, want) {
			t.Errorf("the rendering does not carry %q", want)
		}
	}

	// Both operations that create a resource answer with the header the
	// resource is addressed by.
	if n := strings.Count(operations, "| `Location` |"); n != 2 {
		t.Errorf("the rendering carries %d Location headers, want 2", n)
	}

	// Every path template of this contract declares its parameter on the path
	// item, so the operations under it are the ones that would lose the rows.
	project := section(t, operations, "### `GET /api/v1/projects/{id}`")
	if !strings.Contains(project, "| `id` | `path` | yes |") {
		t.Errorf("the project operation carries no row for the path parameter:\n%s", project)
	}

	// The contract folds its descriptions, so a paragraph of one arrives with a
	// single newline in front of it and is rendered with the blank line that
	// makes it a paragraph of its own.
	events := section(t, operations, "### `GET /api/v1/events`")
	if !strings.Contains(events, "\n\nOne call answers one page.") {
		t.Errorf("the events operation runs its paragraphs together:\n%s", events)
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
