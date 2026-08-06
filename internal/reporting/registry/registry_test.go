package registry_test

import (
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/b42labs/tally/internal/reporting/registry"
)

// instanceSizeSchema is the schema migration 0002 registers for
// (openstack, instance). The unit tests compile the seeded document itself so
// that a change to it that no longer says what it means shows up here and not
// only against a database.
const instanceSizeSchema = `{
    "type": "object",
    "required": ["vcpus", "ram_gb", "disk_gb", "flavor"],
    "properties": {
        "vcpus": {"type": "integer", "minimum": 1},
        "ram_gb": {"type": "number", "exclusiveMinimum": 0},
        "disk_gb": {"type": "number", "minimum": 0},
        "flavor": {"type": "string"}
    },
    "additionalProperties": true
}`

func TestCompile(t *testing.T) {
	t.Run("accepts a draft 2020-12 schema that names no dialect", func(t *testing.T) {
		if _, err := registry.Compile([]byte(instanceSizeSchema)); err != nil {
			t.Fatalf("Compile() error = %v, want nil", err)
		}
	})

	t.Run("rejects a document that is no schema", func(t *testing.T) {
		for name, raw := range map[string]string{
			// A type is a string or an array of them, so 42 is not a keyword value
			// the compiler accepts.
			"a keyword of the wrong type":     `{"type": 42}`,
			"a pattern that does not compile": `{"type": "string", "pattern": "["}`,
			"not JSON at all":                 `{`,
		} {
			t.Run(name, func(t *testing.T) {
				schema, err := registry.Compile([]byte(raw))
				if err == nil {
					t.Fatalf("Compile(%s) error = nil, want the compiler's complaint", raw)
				}
				if schema != nil {
					t.Errorf("Compile(%s) schema = %v, want nil", raw, schema)
				}
			})
		}
	})

	t.Run("carries the compiler's message", func(t *testing.T) {
		_, err := registry.Compile([]byte(`{"type": 42}`))
		if err == nil {
			t.Fatal("Compile() error = nil, want the compiler's complaint")
		}
		// The operator who sent the schema has to learn which keyword is at fault,
		// so the compiler's own message has to survive the wrapping.
		for _, want := range []string{"not valid against metaschema", "/type"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Compile() error = %q, want it to contain %q", err, want)
			}
		}
	})

	t.Run("the seeded instance schema decides what a size may look like", func(t *testing.T) {
		schema, err := registry.Compile([]byte(instanceSizeSchema))
		if err != nil {
			t.Fatalf("Compile() error = %v, want nil", err)
		}

		for name, tc := range map[string]struct {
			size  string
			valid bool
		}{
			"a complete size":               {`{"vcpus": 4, "ram_gb": 8, "disk_gb": 40, "flavor": "m1.large"}`, true},
			"a size with an extra property": {`{"vcpus": 1, "ram_gb": 2, "disk_gb": 0, "flavor": "m1.tiny", "az": "nova"}`, true},
			"vcpus that are not a number":   {`{"vcpus": "four", "ram_gb": 8, "disk_gb": 40, "flavor": "m1.large"}`, false},
			"a size that names no flavor":   {`{"vcpus": 4, "ram_gb": 8, "disk_gb": 40}`, false},
			"a fractional vcpu count":       {`{"vcpus": 1.5, "ram_gb": 8, "disk_gb": 40, "flavor": "m1.large"}`, false},
			"no memory at all":              {`{"vcpus": 4, "ram_gb": 0, "disk_gb": 40, "flavor": "m1.large"}`, false},
			"a size that is not an object":  {`[]`, false},
		} {
			t.Run(name, func(t *testing.T) {
				err := schema.Validate(instance(t, tc.size))
				if tc.valid && err != nil {
					t.Errorf("Validate(%s) error = %v, want nil", tc.size, err)
				}
				if !tc.valid && err == nil {
					t.Errorf("Validate(%s) error = nil, want the rejection", tc.size)
				}
			})
		}
	})
}

// instance decodes a size the way a caller of the validator has to: through the
// library's own decoder, which reads numbers as json.Number rather than as
// float64 and so lets "integer" mean what it says.
func instance(t *testing.T, raw string) any {
	t.Helper()

	value, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("decoding the instance %s: %v", raw, err)
	}
	return value
}
