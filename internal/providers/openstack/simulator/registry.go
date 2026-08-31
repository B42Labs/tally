// This file posts what RegistrationsOf decided to the project registry of the
// Reporting API: a row per project first, the relations between them second.
// The simulator is a client of that API the way the collector's sender is, so
// the documents on the wire are mirrored here rather than imported, and a
// registration carries no retry: an operator who reruns the seed, the period
// and the cloud registers the same rows again, and the ones an earlier run got
// through are found in place.
package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The bounds one registration request is taken under. They repeat the sender's,
// in sender.go, because the simulator posts to the same API and shares no code
// with the collector.
const (
	// registrationTimeout bounds one request. A destination that accepts the
	// connection and then stops answering would otherwise hold the run for as
	// long as it stays that way.
	registrationTimeout = 30 * time.Second
	// refusalBodyMax is how much of a refused answer the error quotes. What
	// arrives there is whatever answered, which may be a proxy's HTML error page
	// rather than the API's problem document.
	refusalBodyMax = 512
	// answerBodyMax bounds an accepted answer that is decoded. A registered
	// project is a few hundred bytes, so a megabyte covers every answer the
	// Reporting API gives. What it rules out is a destination that answers 201
	// and then streams for as long as the timeout allows.
	answerBodyMax = 1 << 20
)

// projectsRoute is the registry route, appended to the configured base URL of
// the Reporting API. The relations of a project hang below it.
const projectsRoute = "/api/v1/projects"

// adminTokenHint is added to the message of an answer that refused the token.
// Registering is an admin operation, so a token that delivers notifications is
// not one that registers the projects they arrive under.
const adminTokenHint = "; TALLY_SIM_API_TOKEN has to be an api token of role admin"

// createProjectDocument is the body POST /api/v1/projects takes. It mirrors the
// documented contract instead of importing the server's type, the way sender.go
// mirrors the ingest result: the simulator is a client of that API, and
// importing the server would pull its router, its schema validation and its
// database driver into this binary.
type createProjectDocument struct {
	Platform   string         `json:"platform"`
	Cloud      string         `json:"cloud"`
	ExternalID string         `json:"external_id"`
	Name       string         `json:"name"`
	Metadata   map[string]any `json:"metadata"`
}

// createRelationDocument is the body POST /api/v1/projects/{id}/relations
// takes. The source is the project the route names, so the body carries the
// target alone.
type createRelationDocument struct {
	TargetID     uuid.UUID      `json:"target_id"`
	RelationType string         `json:"relation_type"`
	Metadata     map[string]any `json:"metadata"`
	ValidFrom    string         `json:"valid_from"`
}

// registeredRelation is a relation as the registry answers it, as much of it as
// a registration reads: which project it reaches, the id the route that ends it
// names, and the instant it starts at, which decides whether that route can end
// it before the month being registered begins. Whether it was valid is not read
// off the row, because the answer holds the relations valid at the instant the
// read named.
type registeredRelation struct {
	ID        uuid.UUID `json:"id"`
	TargetID  uuid.UUID `json:"target_id"`
	ValidFrom time.Time `json:"valid_from"`
}

// relationList is what GET /api/v1/projects/{id}/relations answers: the
// relations of one project as they stood at the instant the request named.
type relationList struct {
	Items []registeredRelation `json:"items"`
}

// registeredProject is a project as the registry answers it, as much of it as a
// registration reads: the id the relations address it by.
type registeredProject struct {
	ID uuid.UUID `json:"id"`
}

// projectList is one page of GET /api/v1/projects. A lookup by cloud and
// external id has at most one page, because that pair is the key.
type projectList struct {
	Items []registeredProject `json:"items"`
}

// Registrar registers the projects and relations of a month with one
// Reporting API.
type Registrar struct {
	endpoint string // the base URL without a trailing slash
	token    string
	client   *http.Client
	logger   *slog.Logger
}

// RegistrationReport counts what one Register call found and did.
type RegistrationReport struct {
	ProjectsCreated, ProjectsExisting, RelationsCreated, RelationsExisting int
}

// NewRegistrar builds the client that registers with the Reporting API at
// reportingURL, holding token as its credential. One trailing slash is trimmed
// off the URL, so a base URL written either way builds the same routes.
//
// logger may be nil, which logs through the default logger.
func NewRegistrar(reportingURL, token string, logger *slog.Logger) *Registrar {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registrar{
		endpoint: strings.TrimSuffix(reportingURL, "/"),
		token:    token,
		client: &http.Client{
			Timeout: registrationTimeout,
			// A redirect is answered rather than followed. The registry routes are
			// fixed paths under a configured base, so the API documents none, and Go
			// strips the Authorization header across hosts alone: a same-host redirect
			// to http:// would carry the admin token over in the clear, which is what
			// the https check on the configured URL exists to rule out. A followed
			// redirect that kept the token would be no better, because Go turns a
			// redirected POST into a GET on 301, 302 and 303, which makes a
			// registration a read that registers nothing.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
	}
}

// Register posts every project of regs and then every relation between them,
// and reports how many rows it created and how many the registry already held.
//
// The order is what makes it work twice. A relation needs both of its ends
// registered, so the rows go first, and a key the registry already holds is
// answered 409 rather than replaced: that row is looked up by its key and its
// id is what the relations point at. A relation that is already active is
// answered 409 too and left as it stands. Rerunning the same seed, period and
// cloud therefore ends in the registry the first run meant to leave behind,
// which is what an operator does after a run that failed halfway.
//
// A rerun under another seed, period or cloud is not that: its tenants are new
// rows, while the Gardener rows are the ones the earlier run registered, so the
// relations of the two runs would attribute two months to one statement. Such a
// registration is refused at the relation, naming the route that ends the
// earlier one and the instant it has to end at, or, when that relation begins no
// earlier than this month and can therefore not be ended before it, the garden
// cloud of its own this month needs instead.
//
// The first answer no path can use ends the registration, and the report then
// counts what got through before it. A set naming a relation whose ends it does
// not register is refused before the first request, because the rows of a set
// like that are rows without the relation that gives them their meaning.
func (r *Registrar) Register(ctx context.Context, regs Registrations) (RegistrationReport, error) {
	var report RegistrationReport

	registered := make(map[ProjectKey]bool, len(regs.Projects))
	for _, project := range regs.Projects {
		registered[project.Key] = true
	}
	for _, relation := range regs.Relations {
		if !registered[relation.Source] || !registered[relation.Target] {
			source, target := relation.Source, relation.Target
			return report, fmt.Errorf(
				"the relation from (%s, %s) to (%s, %s) names a project the registrations do not hold",
				source.Cloud, source.ExternalID, target.Cloud, target.ExternalID)
		}
	}

	ids := make(map[ProjectKey]uuid.UUID, len(regs.Projects))
	for _, project := range regs.Projects {
		id, existed, err := r.registerProject(ctx, project)
		if err != nil {
			return report, err
		}
		ids[project.Key] = id
		message := "registered project"
		if existed {
			report.ProjectsExisting++
			message = "project already registered"
		} else {
			report.ProjectsCreated++
		}
		r.logger.Info(message, "platform", project.Platform, "cloud", project.Key.Cloud,
			"external_id", project.Key.ExternalID, "id", id)
	}

	for _, relation := range regs.Relations {
		source, target := ids[relation.Source], ids[relation.Target]
		existed, err := r.relate(ctx, source, target, relation)
		if err != nil {
			return report, err
		}
		message := "related"
		if existed {
			report.RelationsExisting++
			message = "relation already active"
		} else {
			report.RelationsCreated++
		}
		r.logger.Info(message, "source", source, "target", target,
			"relation_type", relation.RelationType)
	}

	return report, nil
}

// registerProject registers one project and returns the id it is now addressed
// by, and whether the registry already held it.
func (r *Registrar) registerProject(ctx context.Context, project ProjectRegistration) (uuid.UUID, bool, error) {
	status, body, err := r.do(ctx, http.MethodPost, projectsRoute, nil, createProjectDocument{
		Platform:   project.Platform,
		Cloud:      project.Key.Cloud,
		ExternalID: project.Key.ExternalID,
		Name:       project.Name,
		Metadata:   project.Metadata,
	})
	if err != nil {
		return uuid.Nil, false, err
	}

	switch status {
	case http.StatusCreated:
		var created registeredProject
		if err := json.Unmarshal(body, &created); err != nil {
			return uuid.Nil, false, fmt.Errorf("decoding the registered project: %w", err)
		}
		// The relations are addressed by this id, so an answer without one leaves
		// the registration nothing to point at.
		if created.ID == uuid.Nil {
			return uuid.Nil, false, errors.New("the Reporting API answered 201 without a project id")
		}
		return created.ID, false, nil
	case http.StatusConflict:
		id, err := r.lookUpProject(ctx, project.Key)
		return id, true, err
	default:
		return uuid.Nil, false, refused(status, http.MethodPost, projectsRoute, body)
	}
}

// lookUpProject reads the id of a row the registry answered 409 for. The key is
// the pair the registry is keyed by, so an answer carrying anything but one row
// describes a registry this registration cannot address.
func (r *Registrar) lookUpProject(ctx context.Context, key ProjectKey) (uuid.UUID, error) {
	query := url.Values{"cloud": {key.Cloud}, "external_id": {key.ExternalID}}
	status, body, err := r.do(ctx, http.MethodGet, projectsRoute, query, nil)
	if err != nil {
		return uuid.Nil, err
	}
	if status != http.StatusOK {
		return uuid.Nil, refused(status, http.MethodGet, projectsRoute, body)
	}

	var list projectList
	if err := json.Unmarshal(body, &list); err != nil {
		return uuid.Nil, fmt.Errorf("decoding the listed projects: %w", err)
	}
	switch len(list.Items) {
	case 1:
		return list.Items[0].ID, nil
	case 0:
		return uuid.Nil, fmt.Errorf(
			"the Reporting API refused (%s, %s) as registered and lists no such project",
			key.Cloud, key.ExternalID)
	default:
		return uuid.Nil, fmt.Errorf("the Reporting API lists %d projects for (%s, %s), want one",
			len(list.Items), key.Cloud, key.ExternalID)
	}
}

// relate creates one relation and reports whether it was already active. The
// answer carries the stored relation, which nothing here reads: the ids it
// holds are the two this call passed.
func (r *Registrar) relate(ctx context.Context, source, target uuid.UUID,
	relation RelationRegistration,
) (bool, error) {
	route := projectsRoute + "/" + source.String() + "/relations"
	start := relation.ValidFrom.UTC().Format(time.RFC3339)
	// The registry keys an open relation by (source, target, type), so a source
	// already related to another project is not a conflict: the creation would
	// succeed and leave two relations attributing side by side. The export then
	// walks both and sums two months of a tenant's cost into one statement. That
	// is what an earlier run under another period, seed or cloud leaves behind:
	// the Gardener row is keyed by its name and stays, while the tenant it points
	// at is keyed by an identifier the month is salted into.
	//
	// The check is advisory and not a safety property: the registry enforces one
	// open relation per (source, target, type) and nothing below this read stops
	// two registrations that both read an empty list from both creating one. The
	// invariant it stands in for belongs to the Reporting API, as one open
	// relation per (source, type) of an attributing type, answered 409.
	//
	// The relations are read at two instants, because the registry answers the
	// ones valid at one alone. The relation this run creates is valid from the
	// start of its period and has no end, so anything of its type that attributes
	// at or after that start stands beside it. The read at the start of the period
	// is one instant the engine bills the period by: it finds a relation that was
	// closed afterwards too, and DELETE closes one at now, which is after every
	// instant of a simulated month. The read at now finds one that starts after
	// this period and would attribute beside this one in every month from there
	// on.
	//
	// Two point-in-time reads are less than the overlap the engine bills by
	// (valid_from < period_to AND (valid_to IS NULL OR valid_to > period_from)),
	// and the gap between them is a second shape this read lets through: a
	// relation that starts inside the period and was closed no later than now is
	// valid at neither instant and attributes the period all the same. Closing it
	// would need an overlap filter on the route, which the Reporting API does not
	// offer, or the invariant above, which is where it belongs.
	for _, at := range []string{start, ""} {
		stale, err := r.relationBesides(ctx, source, target, relation.RelationType, at)
		if err != nil {
			return false, err
		}
		if stale.ID != uuid.Nil {
			// PATCH refuses a valid_to that is not after the stored valid_from with
			// 422, so ending the earlier relation where this month begins is a way out
			// only while that relation began earlier. When it began at or after this
			// month, the registry holds no instant it could be ended at, and naming
			// the route would send an operator into that refusal.
			if !stale.ValidFrom.Before(relation.ValidFrom.UTC()) {
				return false, fmt.Errorf(
					"%s is already related to %s by %s from %s on, and this run relates it to %s from "+
						"%s on: two relations attributing at once put both tenants into one statement; "+
						"that relation cannot be ended before it starts, so register this month under "+
						"another garden cloud",
					source, stale.TargetID, relation.RelationType,
					stale.ValidFrom.UTC().Format(time.RFC3339), target, start)
			}
			return false, fmt.Errorf(
				"%s is already related to %s by %s, and this run relates it to %s from %s on: two "+
					"relations attributing at once put both tenants into one statement; end the "+
					"earlier one no later than %s with PATCH %s/%s, or register this month under "+
					"another garden cloud",
				source, stale.TargetID, relation.RelationType, target, start, start, route, stale.ID)
		}
	}

	status, body, err := r.do(ctx, http.MethodPost, route, nil, createRelationDocument{
		TargetID:     target,
		RelationType: relation.RelationType,
		Metadata:     relation.Metadata,
		ValidFrom:    start,
	})
	if err != nil {
		return false, err
	}

	switch status {
	case http.StatusCreated:
		return false, nil
	case http.StatusConflict:
		return true, nil
	default:
		return false, refused(status, http.MethodPost, route, body)
	}
}

// relationBesides reads the relations of source that were valid at at, and
// answers the first one of relationType that reaches a project other than
// target. An empty at reads them as they stand now, which is what the registry
// answers a request naming no instant. A zero value is a source this run may
// relate at that instant: either it carries no such relation, or the one it
// carries is the relation this run would create.
func (r *Registrar) relationBesides(ctx context.Context, source, target uuid.UUID,
	relationType, at string,
) (registeredRelation, error) {
	route := projectsRoute + "/" + source.String() + "/relations"
	query := url.Values{"direction": {"outgoing"}, "relation_type": {relationType}}
	if at != "" {
		query.Set("at", at)
	}
	status, body, err := r.do(ctx, http.MethodGet, route, query, nil)
	if err != nil {
		return registeredRelation{}, err
	}
	if status != http.StatusOK {
		return registeredRelation{}, refused(status, http.MethodGet, route, body)
	}

	var list relationList
	if err := json.Unmarshal(body, &list); err != nil {
		return registeredRelation{}, fmt.Errorf("decoding the relations of a project: %w", err)
	}
	for _, stored := range list.Items {
		if stored.TargetID != target {
			return stored, nil
		}
	}
	return registeredRelation{}, nil
}

// do sends one request to route and returns the status and the answer. A body
// is encoded as JSON, and a nil one sends none, which is what tells the two
// posts from the lookup.
func (r *Registrar) do(ctx context.Context, method, route string, query url.Values, body any,
) (int, []byte, error) {
	target := r.endpoint + route
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding the body of %s %s: %w", method, route, err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, route, err)
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := r.client.Do(request)
	if err != nil {
		// The cause stays wrapped, so a caller that cancelled the context reads its
		// own cancellation back out of the error rather than a message about it.
		return 0, nil, fmt.Errorf("%s %s: %w", method, route, err)
	}
	// Reading the rest of the answer before closing it is what lets the connection
	// be used again by the request that follows, of which there are as many as the
	// month has projects and relations.
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	answer, err := io.ReadAll(io.LimitReader(response.Body, answerBodyMax))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the answer of %s %s: %w", method, route, err)
	}
	return response.StatusCode, answer, nil
}

// refused is the error an answer no caller can use ends the registration with.
// It quotes the beginning of the body, because what answered may be a proxy in
// front of the API rather than the API itself, and it names the route without
// the host and the query, so that a message can be pasted into a ticket.
func refused(status int, method, route string, body []byte) error {
	message := fmt.Sprintf("the Reporting API answered %d for %s %s: %s",
		status, method, route, body[:min(len(body), refusalBodyMax)])
	// A refused token is the one failure whose fix is not in the answer: the
	// registration needs a role beyond the one that delivers notifications.
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		message += adminTokenHint
	}
	return errors.New(message)
}
