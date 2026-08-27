package adjustment_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/b42labs/tally/internal/core/adjustment"
)

// conceptExample is the pair of adjustments the concept spells for a reseller
// relation. Validating it also proves the embedded schema compiles.
const conceptExample = `[
	{"type":"discount","description":"Reseller end-customer discount","rate":"0.15","scope":"all"},
	{"type":"kickback","description":"Reseller commission on net revenue","rate":"0.10","scope":"all"}
]`

func TestValidateAcceptsTheConceptExample(t *testing.T) {
	if err := adjustment.Validate([]byte(conceptExample)); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestValidateAccepts(t *testing.T) {
	longDescription := strings.Repeat("x", 500)

	tests := []struct {
		name string
		doc  string
	}{
		{
			name: "the type discount",
			doc:  `[{"type":"discount","rate":"0.15","scope":"all"}]`,
		},
		{
			name: "the type kickback",
			doc:  `[{"type":"kickback","rate":"0.15","scope":"all"}]`,
		},
		{
			name: "the type surcharge",
			doc:  `[{"type":"surcharge","rate":"0.15","scope":"all"}]`,
		},
		{
			name: "the type project_discount",
			doc:  `[{"type":"project_discount","rate":"0.15","scope":"all"}]`,
		},
		{
			name: "the rate zero",
			doc:  `[{"type":"discount","rate":"0","scope":"all"}]`,
		},
		{
			name: "the rate one",
			doc:  `[{"type":"discount","rate":"1","scope":"all"}]`,
		},
		{
			name: "a rate with one decimal place",
			doc:  `[{"type":"discount","rate":"0.5","scope":"all"}]`,
		},
		{
			name: "the smallest rate the pattern admits",
			doc:  `[{"type":"discount","rate":"0.000001","scope":"all"}]`,
		},
		{
			name: "a rate with six decimal places",
			doc:  `[{"type":"discount","rate":"0.123456","scope":"all"}]`,
		},
		{
			name: "one written out to six decimal places",
			doc:  `[{"type":"discount","rate":"1.000000","scope":"all"}]`,
		},
		{
			name: "the scope all",
			doc:  `[{"type":"discount","rate":"0.15","scope":"all"}]`,
		},
		{
			name: "a platform scope",
			doc:  `[{"type":"discount","rate":"0.15","scope":"openstack"}]`,
		},
		{
			name: "a platform and resource type scope",
			doc:  `[{"type":"discount","rate":"0.15","scope":"openstack.instance"}]`,
		},
		{
			name: "a scope with underscores on both sides",
			doc:  `[{"type":"discount","rate":"0.15","scope":"gardener_dev.shoot_node"}]`,
		},
		{
			name: "a description of the longest admitted length",
			doc: fmt.Sprintf(
				`[{"type":"discount","rate":"0.15","scope":"all","description":%q}]`, longDescription),
		},
		{
			name: "an element without a description",
			doc:  `[{"type":"discount","rate":"0.15","scope":"all"}]`,
		},
		{
			name: "as many adjustments as the cap admits",
			doc:  repeatedElements(adjustment.MaxAdjustments),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := adjustment.Validate([]byte(tc.doc)); err != nil {
				t.Errorf("Validate() error = %v, want nil", err)
			}
		})
	}
}

func TestValidateRefuses(t *testing.T) {
	tests := []struct {
		name     string
		doc      string
		location string
		message  string
	}{
		{
			name:    "an empty array",
			doc:     `[]`,
			message: "minItems: got 0, want 1",
		},
		{
			name:    "the null document",
			doc:     `null`,
			message: "got null, want array",
		},
		{
			name:    "an object instead of an array",
			doc:     `{}`,
			message: "got object, want array",
		},
		{
			name:     "a number instead of an element",
			doc:      `[1]`,
			location: "/0",
			message:  "got number, want object",
		},
		{
			name:     "an element without a rate",
			doc:      `[{"type":"discount","scope":"all"}]`,
			location: "/0",
			message:  "missing property 'rate'",
		},
		{
			name:     "a rate written as a number",
			doc:      `[{"type":"discount","rate":0.15,"scope":"all"}]`,
			location: "/0/rate",
			message:  "got number, want string",
		},
		{
			name:     "a rate with seven decimal places",
			doc:      `[{"type":"discount","rate":"0.1234567","scope":"all"}]`,
			location: "/0/rate",
			message:  "does not match pattern",
		},
		{
			name:     "a rate above one",
			doc:      `[{"type":"discount","rate":"1.5","scope":"all"}]`,
			location: "/0/rate",
			message:  "does not match pattern",
		},
		{
			name:     "a rate without a leading zero",
			doc:      `[{"type":"discount","rate":".5","scope":"all"}]`,
			location: "/0/rate",
			message:  "does not match pattern",
		},
		{
			name:     "a negative rate",
			doc:      `[{"type":"discount","rate":"-0.1","scope":"all"}]`,
			location: "/0/rate",
			message:  "does not match pattern",
		},
		{
			name:     "a rate written as a percentage",
			doc:      `[{"type":"discount","rate":"15%","scope":"all"}]`,
			location: "/0/rate",
			message:  "does not match pattern",
		},
		{
			name:     "an empty rate",
			doc:      `[{"type":"discount","rate":"","scope":"all"}]`,
			location: "/0/rate",
			message:  "does not match pattern",
		},
		{
			name:     "a type the enum does not name",
			doc:      `[{"type":"rebate","rate":"0.15","scope":"all"}]`,
			location: "/0/type",
			message:  "value must be one of",
		},
		{
			name:     "a scope in the wrong case",
			doc:      `[{"type":"discount","rate":"0.15","scope":"Openstack.Instance"}]`,
			location: "/0/scope",
			message:  "does not match pattern",
		},
		{
			name:     "a scope of three parts",
			doc:      `[{"type":"discount","rate":"0.15","scope":"openstack.instance.extra"}]`,
			location: "/0/scope",
			message:  "does not match pattern",
		},
		{
			name:     "a scope with a hyphen",
			doc:      `[{"type":"discount","rate":"0.15","scope":"openstack-eu"}]`,
			location: "/0/scope",
			message:  "does not match pattern",
		},
		{
			name:     "an empty scope",
			doc:      `[{"type":"discount","rate":"0.15","scope":""}]`,
			location: "/0/scope",
			message:  "does not match pattern",
		},
		{
			name: "a description one character too long",
			doc: fmt.Sprintf(
				`[{"type":"discount","rate":"0.15","scope":"all","description":%q}]`,
				strings.Repeat("x", 501)),
			location: "/0/description",
			message:  "maxLength: got 501, want 500",
		},
		{
			name:     "a description written as a number",
			doc:      `[{"type":"discount","rate":"0.15","scope":"all","description":5}]`,
			location: "/0/description",
			message:  "got number, want string",
		},
		{
			name:     "an element with a member the schema does not know",
			doc:      `[{"type":"discount","rate":"0.15","scope":"all","note":"x"}]`,
			location: "/0",
			message:  "additional properties 'note' not allowed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := adjustment.Validate([]byte(tc.doc))

			var invalid *adjustment.InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("Validate() error = %v, want *adjustment.InvalidError", err)
			}
			if len(invalid.Violations) != 1 {
				t.Fatalf("Validate() reported %d violations, want 1: %v", len(invalid.Violations), invalid)
			}

			violation := invalid.Violations[0]
			if violation.Location != tc.location {
				t.Errorf("violation location = %q, want %q", violation.Location, tc.location)
			}
			if !strings.Contains(violation.Message, tc.message) {
				t.Errorf("violation message = %q, want it to contain %q", violation.Message, tc.message)
			}
		})
	}

	// Every element is checked, so one round trip tells an operator about all
	// of the elements that have to change rather than about the first one.
	t.Run("two elements refused at once", func(t *testing.T) {
		doc := `[{"type":"rebate","rate":"0.15","scope":"all"},{"type":"discount","rate":0.15,"scope":"all"}]`

		err := adjustment.Validate([]byte(doc))

		var invalid *adjustment.InvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("Validate() error = %v, want *adjustment.InvalidError", err)
		}
		if len(invalid.Violations) != 2 {
			t.Fatalf("Validate() reported %d violations, want 2: %v", len(invalid.Violations), invalid)
		}

		want := []string{"/0/type", "/1/rate"}
		for i, location := range want {
			if invalid.Violations[i].Location != location {
				t.Errorf("violation %d location = %q, want %q", i, invalid.Violations[i].Location, location)
			}
		}
	})

	// The length is read before the schema walks the array, so an array past
	// the cap costs one violation naming the length rather than one per element
	// it also refuses. That is what keeps a body full of refused elements from
	// being answered with a response that grows with it.
	t.Run("more adjustments than the cap admits", func(t *testing.T) {
		over := adjustment.MaxAdjustments + 1
		doc := "[" + strings.TrimSuffix(strings.Repeat("1,", over), ",") + "]"

		err := adjustment.Validate([]byte(doc))

		var invalid *adjustment.InvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("Validate() error = %v, want *adjustment.InvalidError", err)
		}
		if len(invalid.Violations) != 1 {
			t.Fatalf("Validate() reported %d violations, want the length alone: %v",
				len(invalid.Violations), invalid)
		}

		violation := invalid.Violations[0]
		if violation.Location != "" {
			t.Errorf("violation location = %q, want none", violation.Location)
		}
		if want := fmt.Sprintf("maxItems: got %d, want %d", over, adjustment.MaxAdjustments); violation.Message != want {
			t.Errorf("violation message = %q, want %q", violation.Message, want)
		}
	})
}

// repeatedElements is an adjustments array of count identical elements, which
// is how the tests around the cap say how long an array is without spelling it
// out element by element.
func repeatedElements(count int) string {
	return "[" + strings.TrimSuffix(
		strings.Repeat(`{"type":"discount","rate":"0.15","scope":"all"},`, count), ",") + "]"
}

func TestValidateRefusesTextThatIsNotJSON(t *testing.T) {
	tests := []struct {
		name string
		doc  []byte
	}{
		{name: "a truncated array", doc: []byte("[")},
		{name: "no document at all", doc: nil},
		{name: "an empty document", doc: []byte{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := adjustment.Validate(tc.doc)
			if err == nil {
				t.Fatal("Validate() error = nil, want a decoding error")
			}
			if !strings.Contains(err.Error(), "decoding the pricing adjustments") {
				t.Errorf("Validate() error = %q, want it to contain %q", err, "decoding the pricing adjustments")
			}

			// Text that is not JSON never reached the schema, so it must not
			// look to a caller like a document the schema refused.
			var invalid *adjustment.InvalidError
			if errors.As(err, &invalid) {
				t.Errorf("Validate() error = %v, want no *adjustment.InvalidError", err)
			}
		})
	}
}

func TestInvalidErrorNamesEveryViolation(t *testing.T) {
	t.Run("a violation inside an element", func(t *testing.T) {
		err := adjustment.Validate([]byte(`[{"type":"discount","rate":"1.5","scope":"all"}]`))
		if err == nil {
			t.Fatal("Validate() error = nil, want an *adjustment.InvalidError")
		}

		// The pattern itself is left out of the assertion: the validator renders
		// it with %q, which doubles every backslash of the expression.
		want := "the pricing adjustments do not match the schema: " +
			"pricing_adjustments/0/rate: '1.5' does not match pattern"
		if !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Error() = %q, want it to start with %q", err, want)
		}
	})

	t.Run("a violation of the array itself", func(t *testing.T) {
		err := adjustment.Validate([]byte(`[]`))
		if err == nil {
			t.Fatal("Validate() error = nil, want an *adjustment.InvalidError")
		}

		want := "the pricing adjustments do not match the schema: " +
			"pricing_adjustments: minItems: got 0, want 1"
		if err.Error() != want {
			t.Errorf("Error() = %q, want %q", err, want)
		}
	})

	// Two violations are one message naming both, separated by a semicolon,
	// rather than the first one alone.
	t.Run("a violation in each of two elements", func(t *testing.T) {
		err := adjustment.Validate([]byte(
			`[{"type":"rebate","rate":"0.15","scope":"all"},{"type":"discount","rate":"0.15","scope":""}]`))
		if err == nil {
			t.Fatal("Validate() error = nil, want an *adjustment.InvalidError")
		}

		want := "the pricing adjustments do not match the schema: pricing_adjustments/0/type: "
		if !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Error() = %q, want it to start with %q", err, want)
		}
		// The pattern the second violation names is left out for the reason
		// above, so what is asserted is the place and the separator in front of
		// it.
		if want := "; pricing_adjustments/1/scope: "; !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to name the second violation after %q", err, want)
		}
	})
}

func TestValidateMetadata(t *testing.T) {
	tests := []struct {
		name          string
		metadata      []byte
		wantViolation string
		wantErr       string
	}{
		{name: "no metadata at all", metadata: nil},
		{name: "an empty document", metadata: []byte{}},
		{name: "the null document", metadata: []byte(`null`)},
		{name: "an empty object", metadata: []byte(`{}`)},
		{name: "metadata without the member", metadata: []byte(`{"owner":"team-a"}`)},
		{
			name:          "the member set to null",
			metadata:      []byte(`{"pricing_adjustments":null}`),
			wantViolation: "got null, want array",
		},
		{
			name:          "the member set to an empty array",
			metadata:      []byte(`{"pricing_adjustments":[]}`),
			wantViolation: "minItems: got 0, want 1",
		},
		{
			name: "the member beside other metadata",
			metadata: []byte(
				`{"owner":"team-a","pricing_adjustments":[{"type":"discount","rate":"0.15","scope":"all"}]}`),
		},
		{
			name:     "an array instead of an object",
			metadata: []byte(`[]`),
			wantErr:  "decoding the relation metadata",
		},
		{
			name:     "a number instead of an object",
			metadata: []byte(`1`),
			wantErr:  "decoding the relation metadata",
		},
		{
			name:     "a truncated document",
			metadata: []byte(`{"pricing_adjustments":`),
			wantErr:  "decoding the relation metadata",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := adjustment.ValidateMetadata(tc.metadata)

			switch {
			case tc.wantViolation != "":
				var invalid *adjustment.InvalidError
				if !errors.As(err, &invalid) {
					t.Fatalf("ValidateMetadata() error = %v, want *adjustment.InvalidError", err)
				}
				if len(invalid.Violations) != 1 {
					t.Fatalf("ValidateMetadata() reported %d violations, want 1: %v", len(invalid.Violations), invalid)
				}
				violation := invalid.Violations[0]
				if violation.Location != "" {
					t.Errorf("violation location = %q, want none", violation.Location)
				}
				if !strings.Contains(violation.Message, tc.wantViolation) {
					t.Errorf("violation message = %q, want it to contain %q", violation.Message, tc.wantViolation)
				}
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("ValidateMetadata() error = nil, want one containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("ValidateMetadata() error = %q, want it to contain %q", err, tc.wantErr)
				}
			default:
				if err != nil {
					t.Errorf("ValidateMetadata() error = %v, want nil", err)
				}
			}
		})
	}
}

func TestConstantsMatchTheSchema(t *testing.T) {
	t.Run("the metadata key", func(t *testing.T) {
		if adjustment.MetadataKey != "pricing_adjustments" {
			t.Errorf("MetadataKey = %q, want %q", adjustment.MetadataKey, "pricing_adjustments")
		}
	})

	// The literals are compared as they are stored, so a type that only differs
	// in case is a type the schema does not name.
	t.Run("a type in the wrong case", func(t *testing.T) {
		err := adjustment.Validate([]byte(`[{"type":"Discount","rate":"0.15","scope":"all"}]`))

		var invalid *adjustment.InvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("Validate() error = %v, want *adjustment.InvalidError", err)
		}
	})
}

// FuzzValidateMetadata drives the trust boundary of this package: the bytes of
// a relation metadata document as they arrive off a request body. The contract
// admits any object there, so no document may take the API down instead of
// being stored or refused. What this holds up in particular is the walk over
// the validator's detailed output, which reads the message off every leaf it
// reaches, and a refusal has to name what is wrong for a caller to correct it.
func FuzzValidateMetadata(f *testing.F) {
	f.Add([]byte(`{"pricing_adjustments":` + conceptExample + `}`))
	f.Add([]byte(`{"owner":"team-a"}`))
	f.Add([]byte(`{"pricing_adjustments":[{"type":"discount","rate":"1.5","scope":""}]}`))
	f.Add([]byte(`{"pricing_adjustments":[]}`))
	f.Add([]byte(`{"pricing_adjustments":[1,{},{"type":"discount"}]}`))
	f.Add([]byte(`{"pricing_adjustments":` + repeatedElements(adjustment.MaxAdjustments+1) + `}`))
	f.Add([]byte(`{"pricing_adjustments":`))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, metadata []byte) {
		err := adjustment.ValidateMetadata(metadata)

		var invalid *adjustment.InvalidError
		if errors.As(err, &invalid) {
			for i, violation := range invalid.Violations {
				if violation.Message == "" {
					t.Errorf("violation %d of %s names no defect, want the validator's sentence",
						i, metadata)
				}
			}
		}
	})
}
