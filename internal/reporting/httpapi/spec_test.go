package httpapi

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// errBrokenGetter is what a spec getter returns when the embedded document
// cannot even be decoded.
var errBrokenGetter = errors.New("spec is not decodable")

const wrapPrefix = "loading openapi spec:"

// documentPath is the contract this package's embedded copy is generated from.
const documentPath = "../../../api/reporting/openapi.yaml"

// TestEmbeddedSpecMatchesTheDocument catches a contract that was edited without
// running `make generate`. The server routes and validates against the embedded
// copy, so the two drifting apart means the published document describes an API
// that is not the one being served.
func TestEmbeddedSpecMatchesTheDocument(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile(documentPath)
	if err != nil {
		t.Fatalf("loading %s: %v", documentPath, err)
	}
	embedded, err := GetSpec()
	if err != nil {
		t.Fatalf("GetSpec() error = %v, want nil", err)
	}

	want := encodeWithoutOperationIDs(t, document)
	got := encodeWithoutOperationIDs(t, embedded)

	if !bytes.Equal(got, want) {
		t.Errorf("the embedded spec differs from %s, run `make generate`:\nembedded %s\ndocument %s",
			documentPath, got, want)
	}
}

// encodeWithoutOperationIDs renders a document as JSON, minus the operation
// ids: the generator capitalizes them for the Go method names it derives, so
// they are the one member the two copies differ in by design. A renamed
// operation still breaks the build, because the generated interface changes
// with it.
func encodeWithoutOperationIDs(t *testing.T, spec *openapi3.T) []byte {
	t.Helper()

	for _, item := range spec.Paths.Map() {
		for _, operation := range item.Operations() {
			operation.OperationID = ""
		}
	}

	encoded, err := spec.MarshalJSON()
	if err != nil {
		t.Fatalf("encoding the spec: %v", err)
	}
	return encoded
}

func TestLoadSpec(t *testing.T) {
	t.Run("loads and validates the embedded contract", func(t *testing.T) {
		spec, err := loadSpec(GetSpec)
		if err != nil {
			t.Fatalf("loadSpec() error = %v, want nil", err)
		}
		for _, path := range []string{"/healthz", "/readyz"} {
			if spec.Paths.Find(path) == nil {
				t.Errorf("the loaded spec has no %s path", path)
			}
		}
	})

	t.Run("wraps a getter that fails", func(t *testing.T) {
		_, err := loadSpec(func() (*openapi3.T, error) { return nil, errBrokenGetter })
		if !errors.Is(err, errBrokenGetter) {
			t.Fatalf("loadSpec() error = %v, want it to wrap %v", err, errBrokenGetter)
		}
		if !strings.HasPrefix(err.Error(), wrapPrefix) {
			t.Errorf("loadSpec() error = %q, want it to start with %q", err, wrapPrefix)
		}
	})

	t.Run("rejects a document that does not validate", func(t *testing.T) {
		// A document without the openapi version is decodable but invalid, which
		// is what separates this failure from a broken getter.
		_, err := loadSpec(func() (*openapi3.T, error) { return &openapi3.T{}, nil })
		if err == nil {
			t.Fatal("loadSpec() error = nil, want an error")
		}
		if !strings.HasPrefix(err.Error(), wrapPrefix) {
			t.Errorf("loadSpec() error = %q, want it to start with %q", err, wrapPrefix)
		}
		if got := strings.TrimSpace(strings.TrimPrefix(err.Error(), wrapPrefix)); got == "" {
			t.Error("loadSpec() error carries no cause, want the validation failure")
		}
	})
}
