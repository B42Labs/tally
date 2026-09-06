package refdoc

import (
	"testing"
)

// realConstants are the constant groups of this repository a page names, with
// the source that declares them.
var realConstants = []struct {
	path  string
	names []string
}{
	{
		"../reporting/httpapi/problem/problem.go",
		[]string{
			"TypeValidation", "TypeUnauthorized", "TypeForbidden", "TypeNotFound",
			"TypeMethodNotAllowed", "TypeConflict", "TypePayloadTooLarge",
			"TypeHistoryTooLong", "TypeResultTooLarge", "TypeNotImplemented",
			"TypeRelationCycle", "TypeInternal", "TypeUnavailable",
		},
	},
	{
		"../core/event/event.go",
		[]string{
			"eventIDMaxLen", "eventTypeMaxLen", "identifierMaxLen", "stateMaxLen",
			"SourceCollector", "SourceReconciliation",
			"CategoryCreate", "CategoryUpdate", "CategoryDelete",
		},
	},
	{"../providers/openstack/simulator/oracle.go", []string{"oracleFormat"}},
}

func TestConsts(t *testing.T) {
	got, err := Consts(readFixture(t, "constants.go"),
		"TypeValidation", "TypeInternal", "SourceCollector", "identifierMaxLen")
	if err != nil {
		t.Fatalf("Consts() error = %v, want nil", err)
	}

	assertWant(t, "consts.want.md", got)
}

func TestConstsRendersTheConstantsOfTheRepository(t *testing.T) {
	for _, group := range realConstants {
		t.Run(group.path, func(t *testing.T) {
			assertRendersRealSource(t, group.path, func(src []byte) (string, error) {
				return Consts(src, group.names...)
			})
		})
	}
}

func TestConstsRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		names []string
		want  string
	}{
		"no names": {
			names: nil,
			want:  "refdoc: no constants named",
		},
		"absent": {
			names: []string{"absent"},
			want:  "refdoc: no constant absent in the source",
		},
		"declared as a var": {
			names: []string{"notAConstant"},
			want:  "refdoc: no constant notAConstant in the source",
		},
		"repeating the expression above it": {
			names: []string{"counterSecond"},
			want:  "refdoc: constant counterSecond has no value of its own",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Consts(readFixture(t, "constants.go"), tc.names...)
			if err == nil {
				t.Fatalf("Consts() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("Consts() error = %q, want %q", err, tc.want)
			}
		})
	}
}
