package refdoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// defsPrefix is the only kind of reference a schema of this repository writes:
// a definition of the same document. A reference to anything else is refused
// rather than rendered as a link to a heading no page carries.
const defsPrefix = "#/$defs/"

// rootHeading is what a page calls the schema the document declares at its top,
// which has no name of its own the way a definition does.
const rootHeading = "root"

// JSONSchema renders a JSON Schema draft 2020-12 document: a paragraph naming
// the document, then one section per schema object, the root first and the
// definitions after it in the order the file writes them.
//
// A section carrying properties is a table of them with the constraints each
// one is held to, followed by the sentence that says what else the object
// admits. A section without properties is one sentence stating what the value
// is.
func JSONSchema(schema []byte) (string, error) {
	root, err := decodeSchema(schema)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(documentParagraph(root) + "\n")
	writeSchemaSection(&b, rootHeading, root)
	for _, name := range root.Defs.list() {
		writeSchemaSection(&b, name, root.Defs.schemas[name])
	}
	return b.String(), nil
}

// jsonSchema is the part of a schema document the pages render. Every other
// keyword passes through unread.
type jsonSchema struct {
	Title                string                `json:"title"`
	Type                 string                `json:"type"`
	Ref                  string                `json:"$ref"`
	Enum                 []any                 `json:"enum"`
	Const                any                   `json:"const"`
	Pattern              string                `json:"pattern"`
	MinLength            uint64                `json:"minLength"`
	MaxLength            uint64                `json:"maxLength"`
	Minimum              *float64              `json:"minimum"`
	MinItems             uint64                `json:"minItems"`
	MinProperties        uint64                `json:"minProperties"`
	Required             []string              `json:"required"`
	Items                *jsonSchema           `json:"items"`
	OneOf                []*jsonSchema         `json:"oneOf"`
	Not                  *jsonSchema           `json:"not"`
	Properties           *orderedSchemas       `json:"properties"`
	Defs                 *orderedSchemas       `json:"$defs"`
	AdditionalProperties *additionalProperties `json:"additionalProperties"`
}

// renderedKeywords is every keyword the struct above reads, together with
// $schema, which names the dialect and constrains nothing.
//
// A document spelling any other keyword is refused rather than rendered without
// it. A dropped keyword leaves the page stating a weaker contract than the
// validator enforces, and no test notices: the page is compared against this
// same renderer, so a wrong render and a wrong page agree.
var renderedKeywords = map[string]bool{
	"$schema": true, "$ref": true, "$defs": true,
	"title": true, "type": true, "enum": true, "const": true,
	"pattern": true, "minLength": true, "maxLength": true, "minimum": true,
	"minItems": true, "minProperties": true, "required": true,
	"items": true, "oneOf": true, "not": true,
	"properties": true, "additionalProperties": true,
}

// UnmarshalJSON reads the keywords this package renders and refuses the rest.
// The error it returns carries no prefix of its own, because decodeSchema names
// the package and the document once for every error the decoder reports.
func (s *jsonSchema) UnmarshalJSON(data []byte) error {
	// The alias carries the fields and none of the methods, which is what keeps
	// this decode from calling itself.
	type keywords jsonSchema
	if err := json.Unmarshal(data, (*keywords)(s)); err != nil {
		return err
	}

	var spelled map[string]json.RawMessage
	if err := json.Unmarshal(data, &spelled); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(spelled)) {
		if !renderedKeywords[name] {
			return fmt.Errorf("keyword %q is not rendered", name)
		}
	}
	return nil
}

// orderedSchemas is a set of named schemas in the order the document spells
// them. A page lists the definitions of a file and the properties of an object
// the way the file writes them, which a Go map cannot report.
type orderedSchemas struct {
	names   []string
	schemas map[string]*jsonSchema
}

// list returns the names in the order the document writes them. A set the
// document leaves out is nil and names nothing.
func (o *orderedSchemas) list() []string {
	if o == nil {
		return nil
	}
	return o.names
}

// UnmarshalJSON reads the object off the token stream key by key, which is what
// keeps the order the keys arrive in. Its errors carry no prefix of their own:
// decodeSchema names the package and the document once for every error the
// decoder reports.
func (o *orderedSchemas) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	open, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := open.(json.Delim); !ok || delim != '{' {
		return errors.New("a set of named schemas is not an object")
	}

	o.schemas = map[string]*jsonSchema{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return fmt.Errorf("%v is not a name", key)
		}
		var value jsonSchema
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		o.names = append(o.names, name)
		o.schemas[name] = &value
	}
	return nil
}

// additionalProperties is what an object admits beyond the properties it names:
// nothing, anything, or whatever one schema allows.
type additionalProperties struct {
	allowed bool
	schema  *jsonSchema
}

// UnmarshalJSON reads the keyword in both of its shapes, the boolean and the
// schema.
func (a *additionalProperties) UnmarshalJSON(data []byte) error {
	var allowed bool
	if err := json.Unmarshal(data, &allowed); err == nil {
		a.allowed = allowed
		return nil
	}

	var value jsonSchema
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	a.allowed, a.schema = true, &value
	return nil
}

// decodeSchema reads the document and refuses what no page can show: a syntax
// error, a root that states no type, a reference outside the document's own
// definitions, and a not standing where no section states it.
func decodeSchema(raw []byte) (*jsonSchema, error) {
	var root jsonSchema
	if err := json.Unmarshal(raw, &root); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return nil, fmt.Errorf("refdoc: schema: %w at offset %d", syntax, syntax.Offset)
		}
		return nil, fmt.Errorf("refdoc: schema: %w", err)
	}
	if root.Type == "" {
		return nil, errors.New("refdoc: schema has no type")
	}
	if err := checkRefs(&root); err != nil {
		return nil, err
	}
	if err := checkNot(&root, statedAlternatives(&root)); err != nil {
		return nil, err
	}
	return &root, nil
}

// checkRefs walks the whole document once so that the rendering below can turn
// a reference into a link without carrying an error path of its own.
func checkRefs(s *jsonSchema) error {
	if s == nil {
		return nil
	}
	if s.Ref != "" && !strings.HasPrefix(s.Ref, defsPrefix) {
		return fmt.Errorf("refdoc: unsupported $ref %q", s.Ref)
	}
	for _, child := range subSchemas(s) {
		if err := checkRefs(child); err != nil {
			return err
		}
	}
	return nil
}

// checkNot refuses a not no section states in full. The one shape a page
// carries is the not of a oneOf alternative a section writes a bullet for,
// whose required members that bullet names as the properties it forbids. A not
// anywhere else, or one carrying a keyword beyond required, passes the keyword
// check above and then drops out of the page, which leaves the page stating a
// weaker contract than the validator enforces.
func checkNot(s *jsonSchema, stated []*jsonSchema) error {
	if s == nil {
		return nil
	}
	if s.Not != nil {
		switch {
		case !slices.Contains(stated, s):
			return errors.New("refdoc: a not outside a stated oneOf alternative is not rendered")
		case len(s.Not.Required) == 0 || !onlyRequired(s.Not):
			return errors.New("refdoc: a not beyond required is not rendered")
		}
	}
	for _, child := range subSchemas(s) {
		if err := checkNot(child, stated); err != nil {
			return err
		}
	}
	return nil
}

// statedAlternatives is every oneOf member a page writes a bullet for. Only
// writeObject writes the bullets, and only for an object naming properties that
// a section of its own renders: the root, a definition, or the item of an array
// that is one of the two. A oneOf anywhere below that renders as the type word
// of a property or as the sentence of a value, and neither states a not.
func statedAlternatives(root *jsonSchema) []*jsonSchema {
	sections := []*jsonSchema{root}
	for _, name := range root.Defs.list() {
		sections = append(sections, root.Defs.schemas[name])
	}

	var stated []*jsonSchema
	for _, section := range sections {
		object := section
		if section.Type == "array" {
			object = section.Items
		}
		if object != nil && len(object.Properties.list()) > 0 {
			stated = append(stated, object.OneOf...)
		}
	}
	return stated
}

// onlyRequired reports whether required is the only keyword a schema spells.
// The comparison is against the empty schema rather than against a list of the
// other keywords, so a keyword added to the struct above is covered here
// without being named a second time.
func onlyRequired(s *jsonSchema) bool {
	rest := *s
	rest.Required = nil
	return reflect.DeepEqual(rest, jsonSchema{})
}

// subSchemas is every schema one schema carries.
func subSchemas(s *jsonSchema) []*jsonSchema {
	all := append([]*jsonSchema{s.Items, s.Not}, s.OneOf...)
	if s.AdditionalProperties != nil {
		all = append(all, s.AdditionalProperties.schema)
	}
	for _, set := range []*orderedSchemas{s.Properties, s.Defs} {
		for _, name := range set.list() {
			all = append(all, set.schemas[name])
		}
	}
	return all
}

// documentParagraph names the document and says what its root value is.
func documentParagraph(s *jsonSchema) string {
	body := withArticle(s.Type) + "."
	if s.Title == "" {
		return upperFirst(body)
	}
	return code(s.Title) + ", " + body
}

// writeSchemaSection renders one schema object under a heading of its own.
func writeSchemaSection(b *strings.Builder, name string, s *jsonSchema) {
	b.WriteString("\n#### " + code(name) + "\n")

	if s.Type == "array" {
		writeItems(b, s)
		return
	}
	writeObject(b, s)
}

// writeItems renders an array: what one item is, and, when the item names
// properties, the same table and sentences an object of its own gets.
func writeItems(b *strings.Builder, s *jsonSchema) {
	writeBlock(b, itemsSentence(s)+"\n")
	if s.Items != nil && len(s.Items.Properties.list()) > 0 {
		writeObject(b, s.Items)
	}
}

// itemsSentence states what one item of an array is, and what the array itself
// is held to.
func itemsSentence(s *jsonSchema) string {
	if s.Items == nil && s.MinItems == 0 {
		return "The array is unconstrained."
	}

	text := "Each item is unconstrained."
	if s.Items != nil {
		text = "Each item is " + withArticle(schemaTypeWord(s.Items)) + "."
	}
	if s.MinItems > 0 {
		text += " The array holds at least " + countWord(s.MinItems, "item", "items") + "."
	}
	return text
}

// writeObject renders an object: its properties, what it admits beyond them,
// and the alternatives one of which has to hold. An object naming no property
// is one sentence instead.
func writeObject(b *strings.Builder, s *jsonSchema) {
	if len(s.Properties.list()) == 0 {
		writeBlock(b, valueSentence(s)+"\n")
		return
	}

	writeBlock(b, propertyTable(s))
	writeBlock(b, additionalSentence(s)+"\n")
	if list := alternativeList(s); list != "" {
		writeBlock(b, list)
	}
}

// writeBlock writes one paragraph, table or list, with the blank line that
// separates it from whatever stands above it.
func writeBlock(b *strings.Builder, text string) {
	b.WriteString("\n" + text)
}

// propertyTable is one row per property, in the order the document writes them.
func propertyTable(s *jsonSchema) string {
	required := make(map[string]bool, len(s.Required))
	for _, name := range s.Required {
		required[name] = true
	}

	rows := make([][]string, 0, len(s.Properties.names))
	for _, name := range s.Properties.names {
		property := s.Properties.schemas[name]
		rows = append(rows, []string{
			code(name),
			schemaTypeWord(property),
			yesNo(required[name]),
			constraintCell(property),
		})
	}
	return table([]string{"Property", "Type", "Required", "Constraints"}, rows)
}

// additionalSentence says what an object admits beyond the properties it names.
func additionalSentence(s *jsonSchema) string {
	extra := s.AdditionalProperties
	switch {
	case extra == nil || (extra.allowed && extra.schema == nil):
		return "Other properties are allowed."
	case !extra.allowed:
		return "No other property is allowed."
	}

	word := schemaTypeWord(extra.schema)
	if strings.HasPrefix(word, "[") {
		return "Other properties are " + word + "."
	}
	return "Other properties are " + code(word) + "."
}

// alternativeList renders a oneOf on an object as one bullet per alternative,
// naming what that alternative requires and what it pins a property to.
func alternativeList(s *jsonSchema) string {
	var b strings.Builder
	for _, alternative := range s.OneOf {
		b.WriteString("- " + alternativeText(alternative) + "\n")
	}
	return b.String()
}

// alternativeText is one alternative of a oneOf: what it requires, what its not
// member forbids, and what it pins a property to. The three are separate
// conditions, so the one the alternative forbids is stated rather than left to
// be read out of the ones it requires.
func alternativeText(s *jsonSchema) string {
	parts := make([]string, 0, len(s.Required)+len(s.Properties.list()))
	for _, name := range s.Required {
		parts = append(parts, code(name)+" is required")
	}
	if s.Not != nil {
		for _, name := range s.Not.Required {
			parts = append(parts, code(name)+" is absent")
		}
	}
	for _, name := range s.Properties.list() {
		if pinned := s.Properties.schemas[name].Const; pinned != nil {
			parts = append(parts, code(name)+" is "+code(jsonLiteral(pinned)))
		}
	}
	return strings.Join(parts, ", ")
}

// valueSentence states what a value is that names no property: its type with
// its constraints, or, for a oneOf, each alternative in turn.
func valueSentence(s *jsonSchema) string {
	if len(s.OneOf) == 0 {
		return upperFirst(describe(s)) + "."
	}

	alternatives := make([]string, 0, len(s.OneOf))
	for _, alternative := range s.OneOf {
		alternatives = append(alternatives, describe(alternative))
	}
	return upperFirst(joinAlternatives(alternatives)) + "."
}

// describe is the phrase for one value, as "a number at least 0".
func describe(s *jsonSchema) string {
	text := withArticle(schemaTypeWord(s))
	if phrases := constraintPhrases(s); len(phrases) > 0 {
		text += " " + strings.Join(phrases, ", ")
	}
	return text
}

// joinAlternatives lists what one of has to be.
func joinAlternatives(alternatives []string) string {
	if len(alternatives) < 2 {
		return strings.Join(alternatives, "")
	}
	last := len(alternatives) - 1
	return strings.Join(alternatives[:last], ", ") + ", or " + alternatives[last]
}

// schemaTypeWord is the word a table names a schema's type by. A reference is a
// link to the definition's own heading, which is how a reader follows one
// schema object into the next.
func schemaTypeWord(s *jsonSchema) string {
	switch {
	case s.Ref != "":
		name := strings.TrimPrefix(s.Ref, defsPrefix)
		return fmt.Sprintf("[%s](#%s)", name, strings.ToLower(name))
	case s.Type == "array" && s.Items != nil:
		return "array of " + schemaTypeWord(s.Items)
	case s.Type != "":
		return s.Type
	case len(s.Enum) > 0:
		return "enum"
	case len(s.OneOf) > 0:
		return "alternatives"
	}
	return "any"
}

// constraintCell lists what a property is held to, the way a table column
// spells it, or none for a property held to nothing.
func constraintCell(s *jsonSchema) string {
	terms := constraintTerms(s)
	if len(terms) == 0 {
		return "none"
	}
	return strings.Join(terms, "; ")
}

// constraintTerms names each constraint by the keyword that states it, so that
// a reader looks the term up in the schema file itself.
func constraintTerms(s *jsonSchema) []string {
	var terms []string
	if len(s.Enum) > 0 {
		terms = append(terms, codeList(s.Enum))
	}
	if s.Const != nil {
		terms = append(terms, code(jsonLiteral(s.Const)))
	}
	if s.Pattern != "" {
		terms = append(terms, code(s.Pattern))
	}
	// A zero is how an absent bound arrives, and none of these keywords says
	// anything at zero except minimum, which is kept apart for that reason.
	for _, bound := range []struct {
		keyword string
		count   uint64
	}{
		{"minLength", s.MinLength},
		{"maxLength", s.MaxLength},
		{"minItems", s.MinItems},
		{"minProperties", s.MinProperties},
	} {
		if bound.count > 0 {
			terms = append(terms, bound.keyword+" "+strconv.FormatUint(bound.count, 10))
		}
	}
	if s.Minimum != nil {
		terms = append(terms, "minimum "+jsonLiteral(*s.Minimum))
	}
	if values := valuesTerm(s); values != "" {
		terms = append(terms, values)
	}
	return terms
}

// constraintPhrases is the same set of constraints as prose, which is what a
// sentence about a value takes.
func constraintPhrases(s *jsonSchema) []string {
	var phrases []string
	if len(s.Enum) > 0 {
		phrases = append(phrases, "one of "+codeList(s.Enum))
	}
	if s.Const != nil {
		phrases = append(phrases, "exactly "+code(jsonLiteral(s.Const)))
	}
	if s.Pattern != "" {
		phrases = append(phrases, "matching "+code(s.Pattern))
	}
	if s.MinLength > 0 {
		phrases = append(phrases, "of at least "+countWord(s.MinLength, "character", "characters"))
	}
	if s.MaxLength > 0 {
		phrases = append(phrases, "of at most "+countWord(s.MaxLength, "character", "characters"))
	}
	if s.Minimum != nil {
		phrases = append(phrases, "at least "+jsonLiteral(*s.Minimum))
	}
	if s.MinItems > 0 {
		phrases = append(phrases, "of at least "+countWord(s.MinItems, "item", "items"))
	}
	if s.MinProperties > 0 {
		phrases = append(phrases, "with at least "+countWord(s.MinProperties, "property", "properties"))
	}
	return phrases
}

// valuesTerm names what an object admits beyond the properties it declares. It
// recurses, so a map of maps states the constraint of every level.
func valuesTerm(s *jsonSchema) string {
	if s.AdditionalProperties == nil || s.AdditionalProperties.schema == nil {
		return ""
	}

	values := s.AdditionalProperties.schema
	text := "values " + schemaTypeWord(values)
	if nested := constraintTerms(values); len(nested) > 0 {
		text += " (" + strings.Join(nested, "; ") + ")"
	}
	return text
}

// codeList renders literal values as code spans a table cell carries.
func codeList(values []any) string {
	spans := make([]string, 0, len(values))
	for _, value := range values {
		spans = append(spans, code(jsonLiteral(value)))
	}
	return strings.Join(spans, ", ")
}

// jsonLiteral renders a value the way the document writes it, a string without
// the quotes around it and a number without the exponent Go prints floats with.
func jsonLiteral(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return "null"
	}
	return fmt.Sprint(value)
}

// countWord renders a count with the noun it counts.
func countWord(n uint64, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.FormatUint(n, 10) + " " + plural
}

// withArticle puts the article in front of a type word. A link and a code span
// take the article of the word they open with.
func withArticle(word string) string {
	if trimmed := strings.TrimLeft(word, "[`"); trimmed != "" &&
		strings.ContainsRune("aeiou", rune(trimmed[0])) {
		return "an " + word
	}
	return "a " + word
}

// upperFirst opens a sentence with a capital.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
