package refdoc

import (
	"fmt"
	"go/ast"
	"go/types"
)

// fileSuffix names the companion variable of a file-backed secret, the way
// internal/core/envsecret spells it.
const fileSuffix = "_FILE"

// settingsTypes are the field types a settings table renders. The environment
// is parsed into these and nothing else, so a field of another type is a
// mistake in the configuration struct rather than a shape to render.
var settingsTypes = map[string]bool{
	"string":   true,
	"bool":     true,
	"int":      true,
	"int32":    true,
	"int64":    true,
	"[]string": true,
}

// Settings renders the environment variables a configuration struct reads: one
// row per field carrying an env tag, in the order the struct declares them.
// envNames is every variable the package reads, which is where the *_FILE
// companion of a secret is looked up.
//
// A tagged field without a doc comment is refused rather than rendered with an
// empty cell: the comment is what the page tells an operator about the setting,
// and a row without it says only that the variable exists.
func Settings(src []byte, structName string, envNames []string) (string, error) {
	file, err := parseSource(src)
	if err != nil {
		return "", err
	}
	decl, ok := findType(file, structName)
	if !ok {
		return "", fmt.Errorf("refdoc: no type %s in the source", structName)
	}
	structType, ok := decl.spec.Type.(*ast.StructType)
	if !ok {
		return "", fmt.Errorf("refdoc: %s is not a struct", structName)
	}

	known := make(map[string]bool, len(envNames))
	for _, name := range envNames {
		known[name] = true
	}

	var rows [][]string
	for _, field := range structType.Fields.List {
		variable, ok := tagValue(field, "env")
		if !ok || len(field.Names) == 0 {
			continue
		}
		fieldName := field.Names[0].Name

		fieldType := types.ExprString(field.Type)
		if !settingsTypes[fieldType] {
			return "", fmt.Errorf("refdoc: %s.%s: unsupported type %s", structName, fieldName, fieldType)
		}
		governs := joinComment(field.Doc)
		if governs == "" {
			return "", fmt.Errorf("refdoc: %s.%s has no doc comment", structName, fieldName)
		}

		rows = append(rows, []string{
			code(variable),
			goTypeWord(field.Type, nil),
			defaultCell(field),
			fileBackedCell(variable, known),
			governs,
		})
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("refdoc: %s carries no env-tagged field", structName)
	}
	return table([]string{"Variable", "Type", "Default", "File-backed", "Governs"}, rows), nil
}

// defaultCell is the default the field declares, or none for a variable that
// has none and has to be set.
func defaultCell(field *ast.Field) string {
	if value, ok := tagValue(field, "envDefault"); ok && value != "" {
		return code(value)
	}
	return "none"
}

// fileBackedCell says whether the variable has a *_FILE companion, which is how
// a secret reaches the process as a mounted file rather than as a value in a
// pod spec.
func fileBackedCell(variable string, known map[string]bool) string {
	companion := variable + fileSuffix
	if !known[companion] {
		return "no"
	}
	return "yes (" + code(companion) + ")"
}
