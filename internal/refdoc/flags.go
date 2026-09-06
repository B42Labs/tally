package refdoc

import (
	"errors"
	"flag"
)

// Flags renders the flags of a standard library flag set, in the order the set
// reports them. The type comes from the name flag.UnquoteUsage derives, which
// is also what strips the backquotes from the usage.
func Flags(fs *flag.FlagSet) (string, error) {
	if fs == nil {
		return "", errors.New("refdoc: nil flag set")
	}

	var rows [][]string
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		rows = append(rows, []string{
			code("--" + f.Name),
			stdflagTypeWord(name),
			flagDefault(f.DefValue),
			escapePlaceholders(usage),
		})
	})
	if len(rows) == 0 {
		return "This program takes no flags.\n", nil
	}
	return table([]string{"Flag", "Type", "Default", "Description"}, rows), nil
}
