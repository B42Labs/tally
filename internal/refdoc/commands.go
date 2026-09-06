package refdoc

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Commands renders a cobra command tree: one section per command, depth first
// from the root, children in the order the tree holds them.
//
// A command cobra generates rather than the tool declaring it, and one the tree
// hides or deprecates, is left out with everything under it: a page documenting
// them would describe cobra rather than the tool.
func Commands(root *cobra.Command) (string, error) {
	if root == nil {
		return "", errors.New("refdoc: nil command")
	}

	var b strings.Builder
	writeCommand(&b, root)
	return b.String(), nil
}

// writeCommand renders one command and then every command under it.
func writeCommand(b *strings.Builder, cmd *cobra.Command) {
	if skipCommand(cmd) {
		return
	}

	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString("### " + code(cmd.CommandPath()+argumentPart(cmd.Use)) + "\n")
	b.WriteString("\n" + escapePlaceholders(strings.TrimSpace(cmd.Short)) + "\n")
	// A Long that repeats the Short says nothing twice.
	if long := strings.TrimSpace(cmd.Long); long != "" && long != strings.TrimSpace(cmd.Short) {
		b.WriteString("\n" + escapePlaceholders(long) + "\n")
	}
	b.WriteString("\n" + Fenced("text", []byte(cmd.UseLine())))
	if rows := subcommandRows(cmd); len(rows) > 0 {
		b.WriteString("\n" + table([]string{"Subcommand", "Purpose"}, rows))
	}
	b.WriteString("\n" + flagSection(cmd))

	for _, child := range cmd.Commands() {
		writeCommand(b, child)
	}
}

// skipCommand reports whether a command and everything under it stays off the
// page.
func skipCommand(cmd *cobra.Command) bool {
	return !cmd.IsAvailableCommand() || cmd.Name() == "help" || cmd.Name() == "completion"
}

// argumentPart is what the Use line says after the command name: the arguments
// the command takes, with the space that separates them from the name.
func argumentPart(use string) string {
	if i := strings.Index(use, " "); i >= 0 {
		return use[i:]
	}
	return ""
}

// subcommandRows is one row per command below this one, so a reader of a group
// sees what it holds before reading the sections themselves.
func subcommandRows(cmd *cobra.Command) [][]string {
	var rows [][]string
	for _, child := range cmd.Commands() {
		if skipCommand(child) {
			continue
		}
		rows = append(rows, []string{code(child.Name()), escapePlaceholders(child.Short)})
	}
	return rows
}

// flagSection is the table of the flags the command takes itself, or the line
// that says it takes none. The flags of its parents are left to the parent's
// own section.
func flagSection(cmd *cobra.Command) string {
	var rows [][]string
	cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		rows = append(rows, []string{
			code("--" + f.Name),
			pflagTypeWord(f.Value.Type()),
			flagDefault(f.DefValue),
			requiredWord(f),
			escapePlaceholders(f.Usage),
		})
	})
	if len(rows) == 0 {
		return "This command takes no flags.\n"
	}
	return table([]string{"Flag", "Type", "Default", "Required", "Description"}, rows)
}

// requiredWord says whether an invocation has to carry the flag, which is what
// cobra's MarkFlagRequired records on it.
func requiredWord(f *pflag.Flag) string {
	if _, ok := f.Annotations[cobra.BashCompOneRequiredFlag]; ok {
		return "yes"
	}
	return "no"
}

// flagDefault is the value a flag takes when the invocation leaves it out. An
// empty string and an empty list are no value, so both read as none.
func flagDefault(value string) string {
	if value == "" || value == "[]" {
		return "none"
	}
	return code(value)
}
