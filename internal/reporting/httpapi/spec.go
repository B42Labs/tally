package httpapi

import (
	"context"
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"
)

// loadSpec decodes the OpenAPI document and validates it. The request validator
// is only as good as the document behind it, so a document that does not
// validate stops the server from starting rather than silently letting requests
// through unchecked.
//
// The getter is a parameter so that the failure path can be exercised with a
// broken document; production callers pass the generated GetSpec.
func loadSpec(get func() (*openapi3.T, error)) (*openapi3.T, error) {
	spec, err := get()
	if err != nil {
		return nil, fmt.Errorf("loading openapi spec: %w", err)
	}
	if err := spec.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("loading openapi spec: %w", err)
	}
	return spec, nil
}
