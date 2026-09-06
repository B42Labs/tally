package refdoc

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// schemaPage is where the component schemas are documented. An operation links
// the bodies it carries there, and the anchor is what the site derives from the
// heading that names the schema.
const schemaPage = "/reference/api/reporting-api-schemas"

// The two media types this API answers with: one for the documents it serves
// and one for the problem details every error is reported as.
const (
	jsonMediaType    = "application/json"
	problemMediaType = "application/problem+json"
)

// problemResponse is the component every error status of this API references.
const problemResponse = "#/components/responses/Problem"

// requestIDHeader is the header every response of this contract declares. The
// Conventions section of the endpoints page states it once for all of them, so
// a column repeating it on every row would say nothing.
const requestIDHeader = "X-Request-ID"

// schemaRefPrefix opens a reference to a component schema.
const schemaRefPrefix = "#/components/schemas/"

// httpMethods are the verbs an operation section is rendered for, in the order
// the sections follow one another under a path. A verb this API does not use is
// not rendered, so the order is the whole of what decides the page.
var httpMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// OpenAPISecurity renders the credentials the contract declares: one row per
// security scheme, in name order, with what the document says the credential is
// and how it reaches the server.
func OpenAPISecurity(doc *openapi3.T) (string, error) {
	if doc == nil {
		return "", errors.New("refdoc: nil document")
	}

	var rows [][]string
	for _, name := range sortedSchemeNames(doc) {
		scheme := doc.Components.SecuritySchemes[name].Value
		if scheme == nil {
			continue
		}
		rows = append(rows, []string{
			code(name),
			code(strings.TrimSpace(scheme.Type + " " + scheme.Scheme)),
			escapePlaceholders(oneLine(scheme.Description)),
		})
	}
	return table([]string{"Scheme", "Type", "Description"}, rows), nil
}

// sortedSchemeNames is every security scheme of the document in name order.
func sortedSchemeNames(doc *openapi3.T) []string {
	if doc.Components == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(doc.Components.SecuritySchemes))
}

// OpenAPIOperations renders one section per operation: the paths in the order
// the router matches them, and the methods of a path in the fixed order a
// reader walks them.
//
// A section says what the operation does, which credential it takes, what it
// reads off the request, and what every status it answers with carries. The
// bodies link to the schema page rather than repeating the members here, so one
// schema is described in one place.
func OpenAPIOperations(doc *openapi3.T) (string, error) {
	if doc == nil {
		return "", errors.New("refdoc: nil document")
	}
	if doc.Paths == nil || doc.Paths.Len() == 0 {
		return "", errors.New("refdoc: the document declares no paths")
	}

	var b strings.Builder
	for _, path := range doc.Paths.InMatchingOrder() {
		item := doc.Paths.Find(path)
		if item == nil {
			continue
		}
		operations := item.Operations()
		for _, method := range httpMethods {
			operation := operations[method]
			if operation == nil {
				continue
			}
			if err := writeOperation(&b, doc, method, path, item, operation); err != nil {
				return "", err
			}
		}
	}
	return b.String(), nil
}

// writeOperation renders one operation under a heading of its own. The heading
// holds the method and the path in one code span, which is what keeps a path
// template out of the site's heading syntax.
//
// item is the path the operation stands under. A path template declares its
// parameters there rather than on each method, so the operation is rendered
// from both. doc is where an operation that declares no security block of its
// own inherits one.
func writeOperation(b *strings.Builder, doc *openapi3.T, method, path string,
	item *openapi3.PathItem, operation *openapi3.Operation) error {
	credential, err := credentialLine(doc, operation)
	if err != nil {
		return err
	}

	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString("### " + code(method+" "+path) + "\n")

	if summary := strings.TrimSpace(operation.Summary); summary != "" {
		writeBlock(b, escapePlaceholders(summary)+"\n")
	}
	// The contract folds its descriptions, so the break between two paragraphs
	// arrives as a single newline. Each paragraph is rendered as one.
	if description := paragraphs(operation.Description); description != "" {
		writeBlock(b, escapePlaceholders(description)+"\n")
	}
	writeBlock(b, credential+"\n")

	if rows := parameterRows(slices.Concat(item.Parameters, operation.Parameters)); len(rows) > 0 {
		writeBlock(b, table([]string{"Name", "In", "Required", "Type", "Description"}, rows))
	}
	if sentence := requestBodySentence(operation); sentence != "" {
		writeBlock(b, sentence+"\n")
	}
	if rows := responseRows(operation); len(rows) > 0 {
		writeBlock(b, table([]string{"Status", "Description", "Body", "Headers"}, rows))
	}
	return nil
}

// credentialLine names the credentials the operation takes, or says that it
// takes none. The health probes are the operations that take none, which is
// what makes the absence worth stating rather than leaving blank.
//
// An operation without a security block of its own inherits the document's, so
// the absence of a block is not the same as an empty one: only the empty block
// opens the operation up. A requirement naming several schemes asks for all of
// them and several requirements are alternatives, so both are spelled out
// rather than reduced to the first of each, which would tell an integrator that
// an accepted credential is refused.
func credentialLine(doc *openapi3.T, operation *openapi3.Operation) (string, error) {
	requirements := doc.Security
	if operation.Security != nil {
		requirements = *operation.Security
	}
	if len(requirements) == 0 {
		return "No credential.", nil
	}

	alternatives := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		names := slices.Sorted(maps.Keys(requirement))
		if len(names) == 0 {
			return "", fmt.Errorf("refdoc: %s declares an empty security requirement",
				operation.OperationID)
		}
		alternatives = append(alternatives, codeSpans(names, " and "))
	}
	return "Security: " + strings.Join(alternatives, ", or "), nil
}

// parameterRows is one row per parameter, in the order the parameters are
// handed over: the ones the path declares first, the operation's own after
// them.
func parameterRows(parameters openapi3.Parameters) [][]string {
	var rows [][]string
	for _, ref := range parameters {
		parameter := ref.Value
		if parameter == nil {
			continue
		}
		rows = append(rows, []string{
			code(parameter.Name),
			code(parameter.In),
			yesNo(parameter.Required),
			apiTypeWord(parameter.Schema, schemaPageLink),
			escapePlaceholders(oneLine(parameter.Description)),
		})
	}
	return rows
}

// requestBodySentence says what the operation reads off the request, or nothing
// at all for an operation that reads no body.
func requestBodySentence(operation *openapi3.Operation) string {
	if operation.RequestBody == nil || operation.RequestBody.Value == nil {
		return ""
	}
	media := operation.RequestBody.Value.Content[jsonMediaType]
	if media == nil || media.Schema == nil {
		return ""
	}
	return "The request body is " + code(jsonMediaType) + ", " + bodyWords(media.Schema) + "."
}

// bodyWords names what a body carries: one document, an array of them, or the
// alternatives a oneOf leaves open.
func bodyWords(ref *openapi3.SchemaRef) string {
	if name, ok := schemaName(ref); ok {
		return withArticle(schemaPageLink(name))
	}

	value := ref.Value
	switch {
	case value == nil:
		return "a document"
	case value.Type.Is("array"):
		return "an array of " + apiTypeWord(value.Items, schemaPageLink)
	case len(value.OneOf) > 0:
		alternatives := make([]string, 0, len(value.OneOf))
		for _, alternative := range value.OneOf {
			alternatives = append(alternatives, bodyWords(alternative))
		}
		return strings.Join(alternatives, " or ")
	}
	return withArticle(apiTypeWord(ref, schemaPageLink))
}

// responseRows is one row per status the operation answers with, in status
// order.
func responseRows(operation *openapi3.Operation) [][]string {
	if operation.Responses == nil {
		return nil
	}

	var rows [][]string
	for _, status := range operation.Responses.Keys() {
		ref := operation.Responses.Value(status)
		if ref == nil || ref.Value == nil {
			continue
		}
		description := ""
		if ref.Value.Description != nil {
			description = *ref.Value.Description
		}
		rows = append(rows, []string{
			code(status),
			escapePlaceholders(oneLine(description)),
			responseBody(ref),
			responseHeaders(ref),
		})
	}
	return rows
}

// responseBody names what one status carries. Every error of this API is the
// shared problem document, which the media type says more plainly than a link
// to the schema behind it would.
//
// A status carrying a body of another type is named by that type rather than by
// none: the probes and the metrics route answer text/plain, and none is what a
// status without a body at all reads as.
func responseBody(ref *openapi3.ResponseRef) string {
	if ref.Ref == problemResponse || ref.Value.Content[problemMediaType] != nil {
		return code(problemMediaType)
	}
	if media := ref.Value.Content[jsonMediaType]; media != nil && media.Schema != nil {
		return apiTypeWord(media.Schema, schemaPageLink)
	}
	return codeSpans(slices.Sorted(maps.Keys(ref.Value.Content)), ", ")
}

// responseHeaders names the headers one status carries beyond the correlation
// id every response of this contract declares. The Location of a created
// resource is the one this contract states per operation, and a client that
// never learns of it has to guess where what it just created lives.
func responseHeaders(ref *openapi3.ResponseRef) string {
	names := make([]string, 0, len(ref.Value.Headers))
	for _, name := range slices.Sorted(maps.Keys(ref.Value.Headers)) {
		if name != requestIDHeader {
			names = append(names, name)
		}
	}
	return codeSpans(names, ", ")
}

// OpenAPISchemas renders the component schemas: one section per schema in name
// order, with a table of the members it declares. A schema that declares no
// member is one sentence stating what the value is.
func OpenAPISchemas(doc *openapi3.T) (string, error) {
	if doc == nil {
		return "", errors.New("refdoc: nil document")
	}
	if doc.Components == nil {
		return "", nil
	}

	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(doc.Components.Schemas)) {
		value := doc.Components.Schemas[name].Value
		if value == nil {
			continue
		}

		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### " + code(name) + "\n")
		if description := paragraphs(value.Description); description != "" {
			writeBlock(&b, escapePlaceholders(description)+"\n")
		}
		if len(value.Properties) == 0 {
			writeBlock(&b, valueWords(value)+"\n")
			continue
		}
		writeBlock(&b, table(
			[]string{"Property", "Type", "Required", "Description"},
			memberRowsOf(value),
		))
	}
	return b.String(), nil
}

// memberRowsOf is one row per property of a schema, in name order.
func memberRowsOf(value *openapi3.Schema) [][]string {
	required := make(map[string]bool, len(value.Required))
	for _, name := range value.Required {
		required[name] = true
	}

	rows := make([][]string, 0, len(value.Properties))
	for _, name := range slices.Sorted(maps.Keys(value.Properties)) {
		property := value.Properties[name]
		description := ""
		if property.Value != nil {
			description = property.Value.Description
		}
		rows = append(rows, []string{
			code(name),
			apiTypeWord(property, samePageLink),
			yesNo(required[name]),
			escapePlaceholders(oneLine(description)),
		})
	}
	return rows
}

// valueWords states what a schema without properties is, with the format and
// the pattern that pin it down.
func valueWords(value *openapi3.Schema) string {
	text := upperFirst(withArticle(typeName(value)))
	if value.Format != "" {
		text += ", format " + code(value.Format)
	}
	if value.Pattern != "" {
		text += ", matching " + code(value.Pattern)
	}
	return text + "."
}

// apiTypeWord is the word a table names an OpenAPI schema by. link decides
// which page a reference points at, because an operation links to the schema
// page while the schema page links to itself.
func apiTypeWord(ref *openapi3.SchemaRef, link func(string) string) string {
	if ref == nil {
		return "any"
	}
	if name, ok := schemaName(ref); ok {
		return link(name)
	}
	value := ref.Value
	if value == nil {
		return "any"
	}

	var word string
	switch {
	case len(value.Enum) > 0:
		word = codeList(value.Enum)
	case value.Type.Is("array"):
		word = "array of " + apiTypeWord(value.Items, link)
	default:
		word = typeName(value)
	}
	if value.Nullable {
		word += " or null"
	}
	if value.Format != "" {
		word += ", " + code(value.Format)
	}
	if value.Pattern != "" {
		word += ", " + code(value.Pattern)
	}
	if bounds := boundsWords(value); bounds != "" {
		word += ", " + bounds
	}
	// A caller that omits the parameter sends the default, and a caller past a
	// bound is answered 400. Neither is anywhere else on the page.
	if value.Default != nil {
		word += ", default " + code(jsonLiteral(value.Default))
	}
	return word
}

// boundsWords states the range a schema holds a number to, or nothing for one
// it holds to no range. This contract is OpenAPI 3.0, which writes an open end
// as exclusiveMinimum or exclusiveMaximum beside the bound it opens: the open
// and the closed range carry the same minimum and maximum, so an end rendered
// as closed throughout would name an endpoint the server answers 400 to.
func boundsWords(value *openapi3.Schema) string {
	switch {
	case value.Min != nil && value.Max != nil &&
		!value.ExclusiveMin.IsTrue() && !value.ExclusiveMax.IsTrue():
		return jsonLiteral(*value.Min) + " to " + jsonLiteral(*value.Max)
	case value.Min != nil && value.Max != nil:
		return lowBound(value) + " and " + highBound(value)
	case value.Min != nil:
		return lowBound(value)
	case value.Max != nil:
		return highBound(value)
	}
	return ""
}

// lowBound is the bottom of the range: the value itself where the bound is
// closed, and the values past it where it is open.
func lowBound(value *openapi3.Schema) string {
	if value.ExclusiveMin.IsTrue() {
		return "above " + jsonLiteral(*value.Min)
	}
	return "at least " + jsonLiteral(*value.Min)
}

// highBound is the top of the range, worded the way lowBound words the bottom.
func highBound(value *openapi3.Schema) string {
	if value.ExclusiveMax.IsTrue() {
		return "below " + jsonLiteral(*value.Max)
	}
	return "at most " + jsonLiteral(*value.Max)
}

// typeName is the type a schema states, or any for one that states none. The
// batch schema of the ingest path states none on purpose, so a member without a
// type is a shape this document carries rather than an omission.
func typeName(value *openapi3.Schema) string {
	if value.Type == nil || len(*value.Type) == 0 {
		return "any"
	}
	return strings.Join(value.Type.Slice(), " or ")
}

// schemaName is the component schema a reference names, and whether the
// reference names one at all.
func schemaName(ref *openapi3.SchemaRef) (string, bool) {
	if ref == nil || !strings.HasPrefix(ref.Ref, schemaRefPrefix) {
		return "", false
	}
	return strings.TrimPrefix(ref.Ref, schemaRefPrefix), true
}

// schemaPageLink points at the schema's heading on the schema page.
func schemaPageLink(name string) string {
	return fmt.Sprintf("[%s](%s#%s)", name, schemaPage, strings.ToLower(name))
}

// samePageLink points at the schema's heading on the page the link is written
// on.
func samePageLink(name string) string {
	return fmt.Sprintf("[%s](#%s)", name, strings.ToLower(name))
}
