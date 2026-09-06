package refdoc

import (
	"errors"
	"fmt"
	"go/ast"
	"slices"
	"strings"
)

// Struct renders the members of one or more document types: a heading per type,
// what the code says the type is, and a table of the members its tags name.
// tagKey is the tag the wire format is spelled in, json or yaml.
//
// A member whose type is one of the types named in the same call links to that
// type's heading, so a reader follows a document into the one it carries. A
// field without a name, one the tag leaves out, and one tagged - are not part
// of the wire format and are left out of the table.
//
// A comment that names a file or an argument with a placeholder in angle
// brackets has that token rendered in a code span. The site reads a bare <key>
// as markup, so a comment carrying one would fail the build of the page it
// reaches rather than show the name it spells.
func Struct(src []byte, tagKey string, typeNames ...string) (string, error) {
	if len(typeNames) == 0 {
		return "", errors.New("refdoc: no types named")
	}
	file, err := parseSource(src)
	if err != nil {
		return "", err
	}

	linked := make(map[string]bool, len(typeNames))
	for _, name := range typeNames {
		linked[name] = true
	}

	var b strings.Builder
	for _, name := range typeNames {
		decl, ok := findType(file, name)
		if !ok {
			return "", fmt.Errorf("refdoc: no type %s in the source", name)
		}
		structType, ok := decl.spec.Type.(*ast.StructType)
		if !ok {
			return "", fmt.Errorf("refdoc: %s is not a struct", name)
		}

		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("#### " + code(name) + "\n")
		if doc := escapePlaceholders(joinComment(decl.doc)); doc != "" {
			b.WriteString("\n" + doc + "\n")
		}
		if rows := memberRows(structType, tagKey, linked); len(rows) > 0 {
			b.WriteString("\n" + table([]string{"Member", "Type", "Presence", "Description"}, rows))
		}
	}
	return b.String(), nil
}

// memberRows is one row per member the tag names.
func memberRows(structType *ast.StructType, tagKey string, linked map[string]bool) [][]string {
	var rows [][]string
	for _, field := range structType.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		tag, ok := tagValue(field, tagKey)
		if !ok {
			continue
		}
		member, options, _ := strings.Cut(tag, ",")
		if member == "-" {
			continue
		}

		presence := "always"
		if slices.Contains(strings.Split(options, ","), "omitempty") {
			presence = "omitted when empty"
		}
		rows = append(rows, []string{
			code(member),
			goTypeWord(field.Type, linked),
			presence,
			escapePlaceholders(joinComment(field.Doc)),
		})
	}
	return rows
}
