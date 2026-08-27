package adjustment_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

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

func TestParseAcceptsTheConceptExample(t *testing.T) {
	adjustments, err := adjustment.Parse([]byte(conceptExample))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}

	assertAdjustments(t, adjustments, []adjustment.Adjustment{
		{
			Type:        adjustment.TypeDiscount,
			Rate:        decimal.RequireFromString("0.15"),
			Scope:       adjustment.ScopeAll,
			Description: "Reseller end-customer discount",
		},
		{
			Type:        adjustment.TypeKickback,
			Rate:        decimal.RequireFromString("0.10"),
			Scope:       adjustment.ScopeAll,
			Description: "Reseller commission on net revenue",
		},
	})
}

func TestParseReturnsWhatTheTextSpells(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []adjustment.Adjustment
	}{
		{
			name: "an element without a description",
			doc:  `[{"type":"surcharge","rate":"0.123456","scope":"openstack.instance"}]`,
			want: []adjustment.Adjustment{{
				Type:  adjustment.TypeSurcharge,
				Rate:  decimal.RequireFromString("0.123456"),
				Scope: "openstack.instance",
			}},
		},
		{
			// The rate is read off its text, so the places it was written with
			// say nothing about the number it stands for.
			name: "a rate written out to six decimal places",
			doc:  `[{"type":"project_discount","rate":"1.000000","scope":"openstack"}]`,
			want: []adjustment.Adjustment{{
				Type:  adjustment.TypeProjectDiscount,
				Rate:  decimal.RequireFromString("1"),
				Scope: "openstack",
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adjustments, err := adjustment.Parse([]byte(tc.doc))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			assertAdjustments(t, adjustments, tc.want)
		})
	}
}

func TestParseRefuses(t *testing.T) {
	// What the schema refuses, Parse refuses in the same words, so a caller
	// answering 422 reads the place off the violation.
	t.Run("a rate written as a number", func(t *testing.T) {
		adjustments, err := adjustment.Parse([]byte(`[{"type":"discount","rate":0.15,"scope":"all"}]`))
		if adjustments != nil {
			t.Errorf("Parse() = %v, want no adjustments", adjustments)
		}

		var invalid *adjustment.InvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("Parse() error = %v, want *adjustment.InvalidError", err)
		}
		if len(invalid.Violations) != 1 {
			t.Fatalf("Parse() reported %d violations, want 1: %v", len(invalid.Violations), invalid)
		}
		if location := invalid.Violations[0].Location; location != "/0/rate" {
			t.Errorf("violation location = %q, want %q", location, "/0/rate")
		}
	})

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
			adjustments, err := adjustment.Parse(tc.doc)
			if adjustments != nil {
				t.Errorf("Parse() = %v, want no adjustments", adjustments)
			}
			if err == nil {
				t.Fatal("Parse() error = nil, want a decoding error")
			}
			if !strings.Contains(err.Error(), "decoding the pricing adjustments") {
				t.Errorf("Parse() error = %q, want it to contain %q", err, "decoding the pricing adjustments")
			}

			// Text that is not JSON never reached the schema, so it must not
			// look to a caller like a document the schema refused.
			var invalid *adjustment.InvalidError
			if errors.As(err, &invalid) {
				t.Errorf("Parse() error = %v, want no *adjustment.InvalidError", err)
			}
		})
	}
}

// assertAdjustments compares the adjustments a call returned with the ones the
// text spells. The rates are compared with Equal, because a decimal keeps the
// places its text was written with.
func assertAdjustments(t *testing.T, got, want []adjustment.Adjustment) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("got %d adjustments, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Type != want[i].Type {
			t.Errorf("adjustment %d type = %q, want %q", i, got[i].Type, want[i].Type)
		}
		if !got[i].Rate.Equal(want[i].Rate) {
			t.Errorf("adjustment %d rate = %v, want %v", i, got[i].Rate, want[i].Rate)
		}
		if got[i].Scope != want[i].Scope {
			t.Errorf("adjustment %d scope = %q, want %q", i, got[i].Scope, want[i].Scope)
		}
		if got[i].Description != want[i].Description {
			t.Errorf("adjustment %d description = %q, want %q", i, got[i].Description, want[i].Description)
		}
	}
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

func TestFromMetadata(t *testing.T) {
	tests := []struct {
		name          string
		metadata      []byte
		wantFound     bool
		want          []adjustment.Adjustment
		wantViolation string
		wantErr       string
	}{
		{name: "no metadata at all", metadata: nil},
		{name: "an empty document", metadata: []byte{}},
		{name: "the null document", metadata: []byte(`null`)},
		{name: "an empty object", metadata: []byte(`{}`)},
		{name: "metadata without the member", metadata: []byte(`{"owner":"team-a"}`)},
		{
			// The member is there, so the relation is one that adjusts, and
			// the caller hears that beside the refusal.
			name:          "the member set to null",
			metadata:      []byte(`{"pricing_adjustments":null}`),
			wantFound:     true,
			wantViolation: "got null, want array",
		},
		{
			name:          "the member set to an empty array",
			metadata:      []byte(`{"pricing_adjustments":[]}`),
			wantFound:     true,
			wantViolation: "minItems: got 0, want 1",
		},
		{
			name: "the member beside other metadata",
			metadata: []byte(
				`{"owner":"team-a","pricing_adjustments":[{"type":"discount","rate":"0.15","scope":"all"}]}`),
			wantFound: true,
			want: []adjustment.Adjustment{{
				Type:  adjustment.TypeDiscount,
				Rate:  decimal.RequireFromString("0.15"),
				Scope: adjustment.ScopeAll,
			}},
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
			adjustments, found, err := adjustment.FromMetadata(tc.metadata)

			if found != tc.wantFound {
				t.Errorf("FromMetadata() found = %v, want %v", found, tc.wantFound)
			}

			switch {
			case tc.wantViolation != "":
				var invalid *adjustment.InvalidError
				if !errors.As(err, &invalid) {
					t.Fatalf("FromMetadata() error = %v, want *adjustment.InvalidError", err)
				}
				if len(invalid.Violations) != 1 {
					t.Fatalf("FromMetadata() reported %d violations, want 1: %v", len(invalid.Violations), invalid)
				}
				if message := invalid.Violations[0].Message; !strings.Contains(message, tc.wantViolation) {
					t.Errorf("violation message = %q, want it to contain %q", message, tc.wantViolation)
				}
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("FromMetadata() error = nil, want one containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("FromMetadata() error = %q, want it to contain %q", err, tc.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("FromMetadata() error = %v, want nil", err)
				}
				assertAdjustments(t, adjustments, tc.want)
			}
		})
	}
}

func TestScopeMatches(t *testing.T) {
	tests := []struct {
		name         string
		scope        string
		platform     string
		resourceType string
		want         bool
	}{
		{
			name: "the scope all", scope: adjustment.ScopeAll,
			platform: "openstack", resourceType: "instance", want: true,
		},
		{
			name: "a platform scope on its platform", scope: "openstack",
			platform: "openstack", resourceType: "instance", want: true,
		},
		{
			name: "a resource type scope on its resource type", scope: "openstack.instance",
			platform: "openstack", resourceType: "instance", want: true,
		},
		{
			name: "a scope with underscores on both sides", scope: "gardener_dev.shoot_node",
			platform: "gardener_dev", resourceType: "shoot_node", want: true,
		},
		{
			name: "a platform scope on another platform", scope: "openstack",
			platform: "gardener", resourceType: "shoot",
		},
		{
			name: "a resource type scope on another resource type", scope: "openstack.instance",
			platform: "openstack", resourceType: "volume",
		},
		{
			name: "a resource type scope on another platform", scope: "openstack.instance",
			platform: "gardener", resourceType: "instance",
		},
		{
			// The scopes are compared as they are stored, so a scope that only
			// differs in case matches nothing.
			name: "a platform scope in the wrong case", scope: "Openstack",
			platform: "openstack", resourceType: "instance",
		},
		{
			name: "an empty scope", scope: "",
			platform: "openstack", resourceType: "instance",
		},
		{
			name: "a platform scope on no platform at all", scope: "openstack",
			platform: "", resourceType: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := adjustment.ScopeMatches(tc.scope, tc.platform, tc.resourceType)
			if got != tc.want {
				t.Errorf("ScopeMatches(%q, %q, %q) = %v, want %v",
					tc.scope, tc.platform, tc.resourceType, got, tc.want)
			}
		})
	}
}

func TestTypeOrderMatchesTheSchema(t *testing.T) {
	t.Run("the order the adjustments are applied in", func(t *testing.T) {
		want := []string{"surcharge", "discount", "project_discount", "kickback"}
		if !slices.Equal(adjustment.TypeOrder, want) {
			t.Errorf("TypeOrder = %v, want %v", adjustment.TypeOrder, want)
		}
	})

	// Every type the order names is a type the schema admits, so nothing the
	// engine sorts by is a document the API would have refused.
	for _, adjustmentType := range adjustment.TypeOrder {
		t.Run("the type "+adjustmentType, func(t *testing.T) {
			doc := fmt.Sprintf(`[{"type":%q,"rate":"0.15","scope":%q}]`, adjustmentType, adjustment.ScopeAll)

			adjustments, err := adjustment.Parse([]byte(doc))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			assertAdjustments(t, adjustments, []adjustment.Adjustment{{
				Type:  adjustmentType,
				Rate:  decimal.RequireFromString("0.15"),
				Scope: adjustment.ScopeAll,
			}})
		})
	}

	t.Run("a type in the wrong case", func(t *testing.T) {
		_, err := adjustment.Parse([]byte(`[{"type":"Discount","rate":"0.15","scope":"all"}]`))

		var invalid *adjustment.InvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("Parse() error = %v, want *adjustment.InvalidError", err)
		}
	})
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
