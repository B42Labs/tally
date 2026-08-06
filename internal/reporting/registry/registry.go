// Package registry serves the size schemas that resource_types registers per
// (platform, resource_type). A size schema is a JSON Schema draft 2020-12
// document every reported size is checked against, so the handler that stores
// one and the ingest pipeline that applies it both come through here: what the
// handler accepts is exactly what ingest can compile later.
//
// Compiling a schema costs more than validating against it, so a Registry keeps
// the compiled result in memory next to the updated_at of the row it came from.
// A lookup that meets a different updated_at drops the entry and compiles the
// new document. This is what makes an overwrite on one replica visible on every
// other replica at its next lookup, without anything to coordinate between
// them. One entry per (platform, resource_type) keeps the cache bounded by the
// number of registered types rather than by how often they change.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.5.
package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// schemaURL is the location a size schema is compiled under. Nothing loads it:
// it only gives the document an identity and a base for the references it makes
// of its own. The scheme is one no loader serves, so a $ref that resolves
// against it fails to compile instead of reaching for a file on the API's
// filesystem.
const schemaURL = "tally:size-schema"

// Compile turns one size_schema document into a validator. A document without
// a $schema keyword is read as draft 2020-12, which is the dialect the API
// documents and the seeded schemas are written in; a document that names a
// dialect of its own keeps it.
//
// The error of a document that does not compile carries the compiler's own
// message, which names the keyword at fault. Callers put it in front of the
// operator who sent the schema.
func Compile(raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decoding the size schema: %w", err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource(schemaURL, doc); err != nil {
		return nil, fmt.Errorf("adding the size schema: %w", err)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("compiling the size schema: %w", err)
	}
	return schema, nil
}

// Registry hands out the compiled size schema of a registered resource type and
// remembers it for the next caller. Its zero value is not usable; New builds
// one. It is safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	compiled map[typeKey]cacheEntry
}

// typeKey is the pair a resource type is registered under.
type typeKey struct {
	platform     string
	resourceType string
}

// cacheEntry is a compiled schema together with the row version it was compiled
// from. The timestamp is what makes the entry stale, not its age.
type cacheEntry struct {
	schema    *jsonschema.Schema
	updatedAt time.Time
}

// New returns an empty Registry. Every replica keeps its own: the cache holds
// nothing the database does not already hold.
func New() *Registry {
	return &Registry{compiled: map[typeKey]cacheEntry{}}
}

// Validator returns the compiled size schema of (platform, resourceType). The
// second result reports whether the type is registered at all: an unregistered
// pair is not an error here, because ingest rejects the event that names one and
// the handler answers 404 for it.
//
// The schema comes from the cache when the row still carries the updated_at it
// was compiled for, and is compiled and cached otherwise.
func (r *Registry) Validator(ctx context.Context, q *sqlcgen.Queries, platform, resourceType string) (*jsonschema.Schema, bool, error) {
	row, err := q.GetResourceType(ctx, sqlcgen.GetResourceTypeParams{
		Platform:     platform,
		ResourceType: resourceType,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("loading the size schema for (%s, %s): %w", platform, resourceType, err)
	}

	key := typeKey{platform: platform, resourceType: resourceType}
	if schema, ok := r.cached(key, row.UpdatedAt.Time); ok {
		return schema, true, nil
	}

	schema, err := Compile(row.SizeSchema)
	if err != nil {
		// The registering handler compiles before it writes, so a stored document
		// that does not compile means the row was changed past the API.
		return nil, false, fmt.Errorf("the stored size schema for (%s, %s) does not compile: %w", platform, resourceType, err)
	}
	r.remember(key, cacheEntry{schema: schema, updatedAt: row.UpdatedAt.Time})
	return schema, true, nil
}

// cached returns the schema held for key if it was compiled from the row as it
// stands at updatedAt.
func (r *Registry) cached(key typeKey, updatedAt time.Time) (*jsonschema.Schema, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.compiled[key]
	if !ok || !entry.updatedAt.Equal(updatedAt) {
		return nil, false
	}
	return entry.schema, true
}

// remember stores entry as what key currently compiles to.
func (r *Registry) remember(key typeKey, entry cacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.compiled[key] = entry
}
