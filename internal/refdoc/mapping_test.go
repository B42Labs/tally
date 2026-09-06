package refdoc

import (
	"strings"
	"testing"
)

// realMapping is the collector's own mapping table.
const realMapping = "../providers/openstack/mapping.go"

func TestMappingTable(t *testing.T) {
	got, err := MappingTable(readFixture(t, "mapping.go"))
	if err != nil {
		t.Fatalf("MappingTable() error = %v, want nil", err)
	}

	assertWant(t, "mapping.want.md", got)
}

func TestMappingTableRendersEachEntryShape(t *testing.T) {
	got, err := MappingTable(readFixture(t, "mapping.go"))
	if err != nil {
		t.Fatalf("MappingTable() error = %v, want nil", err)
	}

	rows := []string{
		// A state the entry fixes rather than reads, and no size and no skip.
		"| `compute.instance.shelve_offload.end` | `compute.instance.shelve` | `instance` | " +
			"`fixedState(\"shelved\")` | none | `instance_id` | `tenant_id` | none |",
		// A nested path with a fallback, and no project of its own.
		"| `floatingip.id` or `floatingip_id` | request context |",
		// An entry that skips part of what it maps.
		"| `id` | `owner` | `unsizedImage` |",
	}
	for _, row := range rows {
		if !strings.Contains(got, row) {
			t.Errorf("the table does not carry %q:\n%s", row, got)
		}
	}
}

func TestMappingTableRendersTheCollectorsOwnTable(t *testing.T) {
	assertRendersRealSource(t, realMapping, MappingTable)
}

func TestMappingTableRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		src  string
		want string
	}{
		"no literal": {
			src:  "package p\n\nvar other = map[string]int{}\n",
			want: "refdoc: no mappings literal in the source",
		},
		"key is an identifier": {
			src:  "package p\n\nvar mappings = map[string]mappingEntry{\n\tkeyIdent: {eventType: \"x\"},\n}\n",
			want: `refdoc: mappings[keyIdent]: key is not a string literal`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := MappingTable([]byte(tc.src))
			if err == nil {
				t.Fatalf("MappingTable() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("MappingTable() error = %q, want %q", err, tc.want)
			}
		})
	}
}
