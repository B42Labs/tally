// Package problem renders the error responses of the Reporting API: RFC 9457
// documents served as application/problem+json. Every error the API returns
// goes through Write, so clients see one shape whatever failed.
//
// It is a subpackage rather than part of httpapi so that the packages httpapi
// itself imports, such as authentication, can render problems without an import
// cycle.
//
// The normative specification is roadmap/00-conventions.md section 7.
package problem

import (
	"encoding/json"
	"net/http"
)

// The problem types this API uses. A client matches on these URNs rather than
// on the status code, which is why they are part of the contract.
const (
	// TypeValidation marks a request the OpenAPI contract rejects.
	TypeValidation = "urn:tally:error:validation"
	// TypeUnauthorized marks a request that carries no usable credential.
	TypeUnauthorized = "urn:tally:error:unauthorized"
	// TypeForbidden marks an authenticated caller that may not do this.
	TypeForbidden = "urn:tally:error:forbidden"
	// TypeNotFound marks a path or resource that does not exist.
	TypeNotFound = "urn:tally:error:not_found"
	// TypeMethodNotAllowed marks a known path addressed with the wrong method.
	TypeMethodNotAllowed = "urn:tally:error:method_not_allowed"
	// TypeConflict marks a write that collides with existing state, such as a
	// project whose (cloud, external_id) is already registered or a relation
	// triple that is already active.
	TypeConflict = "urn:tally:error:conflict"
	// TypePayloadTooLarge marks a request that carries more than the endpoint
	// takes at once, such as an event batch above the item limit.
	TypePayloadTooLarge = "urn:tally:error:payload_too_large"
	// TypeHistoryTooLong marks a resource whose stored history is longer than
	// the unpaginated per-resource reads answer at once.
	TypeHistoryTooLong = "urn:tally:error:history_too_long"
	// TypeRelationCycle marks a relation creation that would close a cycle over
	// the relation types that attribute cost.
	TypeRelationCycle = "urn:tally:error:relation_cycle"
	// TypeInternal marks a failure the caller cannot do anything about.
	TypeInternal = "urn:tally:error:internal"
	// TypeUnavailable marks a dependency the service needs and cannot reach.
	TypeUnavailable = "urn:tally:error:unavailable"
)

// ContentType is the media type of every problem response.
const ContentType = "application/problem+json"

// FieldError points at one offending value inside a rejected request.
type FieldError struct {
	// Loc is where the value sits, for example body.resource_id.
	Loc string `json:"loc"`
	// Msg says what is wrong with the value at that location.
	Msg string `json:"msg"`
}

// document is the wire shape of a problem response. Detail and Errors are
// dropped when empty so that a problem without them stays minimal.
type document struct {
	Type   string       `json:"type"`
	Title  string       `json:"title"`
	Status int          `json:"status"`
	Detail string       `json:"detail,omitempty"`
	Errors []FieldError `json:"errors,omitempty"`
}

// Write renders one problem: it sets the content type, sends status, and writes
// the document. The status is repeated inside the body, as RFC 9457 asks, so a
// problem stays self-describing once it is logged or forwarded.
//
// Callers pass field errors only for validation failures.
func Write(w http.ResponseWriter, status int, typ, title, detail string, errs ...FieldError) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)

	// The document holds strings and one int, so encoding fails only when the
	// connection does, and the status line is already gone by then.
	_ = json.NewEncoder(w).Encode(document{
		Type:   typ,
		Title:  title,
		Status: status,
		Detail: detail,
		Errors: errs,
	})
}
