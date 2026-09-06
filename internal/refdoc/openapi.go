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
			if operation := operations[method]; operation != nil {
				writeOperation(&b, method, path, operation)
			}
		}
	}
	return b.String(), nil
}

// writeOperation renders one operation under a heading of its own. The heading
// holds the method and the path in one code span, which is what keeps a path
// template out of the site's heading syntax.
func writeOperation(b *strings.Builder, method, path string, operation *openapi3.Operation) {
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString("### " + code(method+" "+path) + "\n")

	if summary := strings.TrimSpace(operation.Summary); summary != "" {
		writeBlock(b, escapePlaceholders(summary)+"\n")
	}
	// The description is kept as the contract writes it, several paragraphs and
	// their markup included.
	if description := strings.TrimSpace(operation.Description); description != "" {
		writeBlock(b, escapePlaceholders(description)+"\n")
	}
	writeBlock(b, credentialLine(operation)+"\n")

	if rows := parameterRows(operation); len(rows) > 0 {
		writeBlock(b, table([]string{"Name", "In", "Required", "Type", "Description"}, rows))
	}
	if sentence := requestBodySentence(operation); sentence != "" {
		writeBlock(b, sentence+"\n")
	}
	if rows := responseRows(operation); len(rows) > 0 {
		writeBlock(b, table([]string{"Status", "Description", "Body"}, rows))
	}
}

// credentialLine names the credential the operation takes, or says that it
// takes none. The health probes are the operations that take none, which is
// what makes the absence worth stating rather than leaving blank.
func credentialLine(operation *openapi3.Operation) string {
	if operation.Security == nil || len(*operation.Security) == 0 {
		return "No credential."
	}
	names := slices.Sorted(maps.Keys((*operation.Security)[0]))
	if len(names) == 0 {
		return "No credential."
	}
	return "Security: " + code(names[0])
}

// parameterRows is one row per parameter the operation declares itself.
func parameterRows(operation *openapi3.Operation) [][]string {
	var rows [][]string
	for _, ref := range operation.Parameters {
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
		return "a " + schemaPageLink(name)
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
	return "a " + apiTypeWord(ref, schemaPageLink)
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
		})
	}
	return rows
}

// responseBody names what one status carries. Every error of this API is the
// shared problem document, which the media type says more plainly than a link
// to the schema behind it would.
func responseBody(ref *openapi3.ResponseRef) string {
	if ref.Ref == problemResponse || ref.Value.Content[problemMediaType] != nil {
		return code(problemMediaType)
	}
	media := ref.Value.Content[jsonMediaType]
	if media == nil || media.Schema == nil {
		return "none"
	}
	return apiTypeWord(media.Schema, schemaPageLink)
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
		if description := oneLine(value.Description); description != "" {
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
	return word
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
