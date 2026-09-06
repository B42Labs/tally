package refdoc

import (
	"strings"
	"testing"
)

// realDocuments are the wire types of this repository, every one of which the
// renderer has to get through.
var realDocuments = []struct {
	path      string
	tagKey    string
	typeNames []string
}{
	{"../core/event/event.go", "json", []string{"Event", "PayloadEnvelope"}},
	{
		"../engine/statements/statements.go", "json",
		[]string{"Document", "BillingPeriod", "LineItem", "Period", "RelatedCost"},
	},
	{
		"../engine/corrections/corrections.go", "json",
		[]string{"CreditNote", "LineItem", "Change", "AdjustmentChange", "RelatedCost"},
	},
	{
		"../engine/export/json.go", "json",
		[]string{"runDocument", "statementEntry", "rollupIndex", "rollupEntry"},
	},
	{
		"../engine/export/kickbacks.go", "json",
		[]string{"kickbacksDocument", "beneficiaryEntry", "kickbackEntry"},
	},
	{"../engine/export/rollup.go", "json", []string{"rollupDocument", "rollupMemberEntry"}},
	{"../engine/adjustments/adjustments.go", "json", []string{"Line"}},
	{"../reporting/reconciliation/config.go", "yaml", []string{"CloudConfig"}},
	{"../engine/counters/counters.go", "yaml", []string{"sourceFile"}},
	{"../providers/openstack/simulator/control.go", "json", []string{"clockDocument"}},
	{"../providers/openstack/simulator/stream.go", "json", []string{"Line"}},
	{
		"../providers/openstack/simulator/oracle.go", "json",
		[]string{"Oracle", "OracleResource", "OracleInterval", "OracleCount", "OracleTraffic"},
	},
	{
		"../../cmd/tally-vertical-slice/compute.go", "json",
		[]string{"document", "periodDoc", "resourceDoc", "recordDoc", "dimensionDoc"},
	},
}

func TestStruct(t *testing.T) {
	got, err := Struct(readFixture(t, "documents.go"), "json", "Document", "Period", "LineItem", "Grouped")
	if err != nil {
		t.Fatalf("Struct() error = %v, want nil", err)
	}

	assertWant(t, "struct.want.md", got)
}

func TestStructRendersEachMemberShape(t *testing.T) {
	got, err := Struct(readFixture(t, "documents.go"), "json", "Document", "Period", "LineItem", "Grouped")
	if err != nil {
		t.Fatalf("Struct() error = %v, want nil", err)
	}

	rows := []string{
		// A type rendered in the same call links to its own heading.
		"| `billing_period` | [Period](#period) | always |",
		"| `line_items` | array of [LineItem](#lineitem) | always |",
		// A pointer is nullable, and omitempty says the member can be absent.
		"| `base_cost` | decimal, 2 places or null | omitted when empty |",
		"| `size` | object | always |",
		"| `stats` | object | always |",
		"| `received_at` | string, RFC 3339 UTC | always |",
		"| `quantities` | object of decimal, 4 places | always |",
	}
	for _, row := range rows {
		if !strings.Contains(got, row) {
			t.Errorf("the table does not carry %q:\n%s", row, got)
		}
	}

	// A field the tag skips, one without a tag, and an embedded one are not
	// part of the wire format.
	for _, absent := range []string{"Skipped", "Legacy", "internal", "`-`"} {
		if strings.Contains(got, absent) {
			t.Errorf("the table carries %q, want it left out:\n%s", absent, got)
		}
	}
}

func TestStructRendersAYAMLType(t *testing.T) {
	got, err := Struct(readFixture(t, "documents.go"), "yaml", "sourceEntry")
	if err != nil {
		t.Fatalf("Struct() error = %v, want nil", err)
	}

	for _, row := range []string{
		"#### `sourceEntry`",
		"| `platform` | string | always |",
		"| `required` | boolean or null | always | Required is a pointer so that an absent key can be told from false. |",
	} {
		if !strings.Contains(got, row) {
			t.Errorf("the table does not carry %q:\n%s", row, got)
		}
	}
}

// TestStructWrapsAPlaceholderInAComment covers the one thing a comment carries
// that the site cannot show as it stands. A page is compiled as a Vue template,
// so a bare <key> is read as a component that never closes and the build of the
// page fails on it. rollupDocument in internal/engine/export names its file that
// way, which is where this came from.
func TestStructWrapsAPlaceholderInAComment(t *testing.T) {
	src := []byte("package testdata\n\n" +
		"// Group is written to rollup-<key>.json beside the statements.\n" +
		"type Group struct {\n" +
		"\t// File is the name statement-<key>.json a member points at.\n" +
		"\tFile string `json:\"file\"`\n" +
		"}\n")

	got, err := Struct(src, "json", "Group")
	if err != nil {
		t.Fatalf("Struct() error = %v, want nil", err)
	}

	for _, want := range []string{
		"Group is written to `rollup-<key>.json` beside the statements.",
		"| `file` | string | always | File is the name `statement-<key>.json` a member points at. |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestStructRendersAStructWithoutMembers(t *testing.T) {
	// Every field is unpublished, so the type gets its heading and its comment
	// and no table at all.
	got, err := Struct(readFixture(t, "documents.go"), "json", "Untagged")
	if err != nil {
		t.Fatalf("Struct() error = %v, want nil", err)
	}

	want := "#### `Untagged`\n\nUntagged carries no member the wire format names.\n"
	if got != want {
		t.Errorf("Struct() = %q, want %q", got, want)
	}
}

func TestStructRendersEveryWireTypeOfTheRepository(t *testing.T) {
	for _, document := range realDocuments {
		t.Run(document.path, func(t *testing.T) {
			assertRendersRealSource(t, document.path, func(src []byte) (string, error) {
				return Struct(src, document.tagKey, document.typeNames...)
			})
		})
	}
}

func TestStructRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		typeNames []string
		want      string
	}{
		"no names":     {typeNames: nil, want: "refdoc: no types named"},
		"no such type": {typeNames: []string{"Absent"}, want: "refdoc: no type Absent in the source"},
		"not a struct": {typeNames: []string{"Key"}, want: "refdoc: Key is not a struct"},
		"one bad name among good ones": {
			typeNames: []string{"Period", "Key"},
			want:      "refdoc: Key is not a struct",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Struct(readFixture(t, "documents.go"), "json", tc.typeNames...)
			if err == nil {
				t.Fatalf("Struct() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("Struct() error = %q, want %q", err, tc.want)
			}
		})
	}
}
