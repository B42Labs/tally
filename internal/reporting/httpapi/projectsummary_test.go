package httpapi

import (
	"reflect"
	"testing"

	"github.com/b42labs/tally/internal/reporting/stats"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// TestMergeActivity pins how the two halves of a summary meet: the window folded
// out of the history and what the project runs today answer different questions,
// so a resource type named by one of them alone is reported with zeros on the
// other side rather than dropped.
func TestMergeActivity(t *testing.T) {
	tests := map[string]struct {
		activities []stats.Activity
		active     []sqlcgen.CountProjectResourcesByTypeRow
		want       []ProjectActivity
	}{
		"a type both sides name carries the window and the present": {
			activities: []stats.Activity{
				{ResourceType: "instance", Created: 2, Deleted: 1, TotalMinutes: 4320},
			},
			active: []sqlcgen.CountProjectResourcesByTypeRow{
				{ResourceType: "instance", Resources: 5},
			},
			want: []ProjectActivity{
				{ResourceType: "instance", ActiveNow: 5, Created: 2, Deleted: 1, TotalMinutes: 4320},
			},
		},
		"a type the window never saw still reports what runs today": {
			activities: nil,
			active: []sqlcgen.CountProjectResourcesByTypeRow{
				{ResourceType: "volume", Resources: 3},
			},
			want: []ProjectActivity{
				{ResourceType: "volume", ActiveNow: 3},
			},
		},
		"a type the project has given up still reports its window": {
			activities: []stats.Activity{
				{ResourceType: "instance", Created: 1, Deleted: 1, TotalMinutes: 60},
			},
			active: nil,
			want: []ProjectActivity{
				{ResourceType: "instance", Created: 1, Deleted: 1, TotalMinutes: 60},
			},
		},
		"the union of both sides comes out ordered by resource type": {
			activities: []stats.Activity{
				{ResourceType: "volume", Created: 1, TotalMinutes: 30},
			},
			active: []sqlcgen.CountProjectResourcesByTypeRow{
				{ResourceType: "instance", Resources: 2},
			},
			want: []ProjectActivity{
				{ResourceType: "instance", ActiveNow: 2},
				{ResourceType: "volume", Created: 1, TotalMinutes: 30},
			},
		},
		"a project with neither events nor resources yields no row": {
			activities: nil,
			active:     nil,
			want:       []ProjectActivity{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := mergeActivity(tc.activities, tc.active)
			if got == nil {
				t.Fatal("rows = nil, want an empty array rather than a null one")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rows = %+v, want %+v", got, tc.want)
			}
		})
	}
}
