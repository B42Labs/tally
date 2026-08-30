package simulator

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// unknownSwitch is the whole error a name outside FaultNames is refused with.
// It lists the six, because a mistyped switch is answered with the ones that
// exist rather than with the name that does not.
const unknownSwitch = `unknown fault switch "bogus"; the switches are ` +
	`pre-existing, missing-create, duplicates, reordering, refused-shapes, held-back`

func TestParseFaults(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		// want is the parsed set and wantNames what it names itself by. Both are
		// read only where wantErr is empty.
		want      Faults
		wantNames []string
		wantErr   string
	}{
		{
			name:      "no switch at all",
			names:     nil,
			want:      Faults{},
			wantNames: []string{},
		},
		{
			name:      "an empty list of switches",
			names:     []string{},
			want:      Faults{},
			wantNames: []string{},
		},
		{
			name:      "one switch on its own",
			names:     []string{FaultDuplicates},
			want:      Faults{Duplicates: true},
			wantNames: []string{FaultDuplicates},
		},
		{
			// Every switch but missing-create, which excludes pre-existing. They
			// come in reverse and one of them twice, which is what the order of
			// Names and the set behind it are read from.
			name: "every switch that stands beside the others, in reverse and one twice",
			names: []string{
				FaultHeldBack, FaultRefusedShapes, FaultReordering,
				FaultDuplicates, FaultPreExisting, FaultHeldBack,
			},
			want: Faults{
				PreExisting: true, Duplicates: true,
				Reordering: true, RefusedShapes: true, HeldBack: true,
			},
			wantNames: []string{
				FaultPreExisting, FaultDuplicates, FaultReordering,
				FaultRefusedShapes, FaultHeldBack,
			},
		},
		{
			name:    "a switch nobody named that way",
			names:   []string{"bogus"},
			wantErr: unknownSwitch,
		},
		{
			name:    "an unknown switch beside a known one",
			names:   []string{FaultDuplicates, "bogus"},
			wantErr: unknownSwitch,
		},
		{
			name:    "the two switches that exclude each other",
			names:   []string{FaultPreExisting, FaultMissingCreate},
			wantErr: "pre-existing and missing-create exclude each other",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseFaults(c.names)

			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseFaults(%q) error = nil, want %q", c.names, c.wantErr)
				}
				if err.Error() != c.wantErr {
					t.Errorf("ParseFaults(%q) error = %q, want %q", c.names, err, c.wantErr)
				}
				if got != (Faults{}) {
					t.Errorf("ParseFaults(%q) = %+v, want the zero Faults beside the error", c.names, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseFaults(%q) error = %v, want nil", c.names, err)
			}
			if got != c.want {
				t.Errorf("ParseFaults(%q) = %+v, want %+v", c.names, got, c.want)
			}
			if names := got.Names(); !slices.Equal(names, c.wantNames) {
				t.Errorf("ParseFaults(%q).Names() = %v, want %v", c.names, names, c.wantNames)
			}
		})
	}
}

// TestFaultsNamesRunInSwitchOrder holds the set with every switch on to
// FaultNames. It is built here rather than parsed, because ParseFaults never
// returns it: pre-existing and missing-create exclude each other.
func TestFaultsNamesRunInSwitchOrder(t *testing.T) {
	all := Faults{
		PreExisting: true, MissingCreate: true, Duplicates: true,
		Reordering: true, RefusedShapes: true, HeldBack: true,
	}
	if got := all.Names(); !slices.Equal(got, FaultNames) {
		t.Errorf("Names() of every switch = %v, want %v", got, FaultNames)
	}
}

// TestFaultsNamesIsNeverNil holds both name lists to the empty slice. They are
// rendered into the oracle, where a nil slice becomes null and an empty one the
// [] whoever reads a month expects.
func TestFaultsNamesIsNeverNil(t *testing.T) {
	if names := (Faults{}).Names(); names == nil || len(names) != 0 {
		t.Errorf("Faults{}.Names() = %#v, want an empty non-nil slice", names)
	}
	if names := (touchedResources{}).names(resourceKey{}); names == nil || len(names) != 0 {
		t.Errorf("touchedResources{}.names(resourceKey{}) = %#v, want an empty non-nil slice", names)
	}
}

func TestTouchedResourcesNameTheSwitchesInOrder(t *testing.T) {
	instanceKey := resourceKey{resourceType: "instance", resourceID: "i-1"}
	volumeKey := resourceKey{resourceType: "volume", resourceID: "v-1"}

	touched := make(touchedResources)
	// Out of FaultNames order and with one switch added twice, which is what the
	// order of the answer and the set behind it are read from.
	touched.add(instanceKey, FaultHeldBack)
	touched.add(instanceKey, FaultPreExisting)
	touched.add(instanceKey, FaultHeldBack)

	want := []string{FaultPreExisting, FaultHeldBack}
	if got := touched.names(instanceKey); !slices.Equal(got, want) {
		t.Errorf("names(%+v) = %v, want %v", instanceKey, got, want)
	}
	if got := touched.names(volumeKey); got == nil || len(got) != 0 {
		t.Errorf("names(%+v) = %#v, want an empty non-nil slice for a resource nothing touched", volumeKey, got)
	}

	g := &generator{touched: make(touchedResources)}
	g.touch(volumeKey.resourceType, volumeKey.resourceID, FaultDuplicates)
	if got := g.touched.names(volumeKey); !slices.Equal(got, []string{FaultDuplicates}) {
		t.Errorf("touch(%q, %q, %q) recorded %v under %+v, want %v",
			volumeKey.resourceType, volumeKey.resourceID, FaultDuplicates, got, volumeKey, []string{FaultDuplicates})
	}
	if got := g.touched.names(instanceKey); len(got) != 0 {
		t.Errorf("touch recorded %v under %+v, want it under %+v alone", got, instanceKey, volumeKey)
	}
}

// TestFaultStreamsAreSeededByTheirName holds a switch's stream to the seed and
// the name together. Two switches drawing one sequence would mean which
// resources one of them touches follows from the other being on beside it.
func TestFaultStreamsAreSeededByTheirName(t *testing.T) {
	first := draws(faultStream(1, FaultPreExisting))

	if again := draws(faultStream(1, FaultPreExisting)); again != first {
		t.Errorf("faultStream(1, %q) drew %v and then %v, want the same draws twice",
			FaultPreExisting, first, again)
	}
	if other := draws(faultStream(1, FaultDuplicates)); other == first {
		t.Errorf("faultStream(1, %q) drew %v, want another sequence than %q's",
			FaultDuplicates, other, FaultPreExisting)
	}
	if other := draws(faultStream(2, FaultPreExisting)); other == first {
		t.Errorf("faultStream(2, %q) drew %v, want another sequence than seed 1's",
			FaultPreExisting, other)
	}
}

// draws takes the first four values of a stream, which is enough to tell two
// generators apart.
func draws(stream *rand.Rand) [4]uint64 {
	var drawn [4]uint64
	for i := range drawn {
		drawn[i] = stream.Uint64()
	}
	return drawn
}
