package export

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/statements"
)

// runFileName is the index every JSON export carries beside its documents.
const runFileName = "run.json"

// The two prefixes a document file carries. What a regular run bills a project
// is a statement, and what a correction hands it is a credit note, so the name
// says which of the two a file holds without opening it.
const (
	statementPrefix  = "statement-"
	creditNotePrefix = "credit-note-"
)

// nameMaxLen is the longest a document file name renders to before it is named
// after its key's digest instead. NAME_MAX is 255 bytes on ext4, XFS and APFS,
// and writeFile hands os.CreateTemp the name with a pattern appended, which
// costs a further fifteen or so.
//
// A statement key has no bound of its own past the 512 bytes an event's cloud
// and project id each carry (internal/core/event: identifierMaxLen), and the
// double escaping below multiplies those: one project id of 240 plain ASCII
// characters, or of 26 characters that are not ASCII, renders past what a file
// name holds.
const nameMaxLen = 200

// JSONFiles writes a run as JSON files into a directory: one document per
// statement, and run.json, which names the run, the pricing version it rated
// with, its stats, and one entry per document beside the file it was written
// to. It is the file implementation of BillingExporter.
type JSONFiles struct {
	// Dir is where the files are written. It is created, with every parent it
	// needs, when the export has rendered everything.
	Dir string
}

// DocumentFileName is the file one statement is written to: the prefix its
// kind carries, the statement key escaped, and .json.
//
// The key is escaped as a whole, and its halves are escaped already, so the
// slash between them becomes %2F and the percent of an escape inside a half
// becomes %25. The double escaping is what keeps the names apart: the cloud
// "os-prod/a" with project "b" and the cloud "os-prod" with project "a/b"
// render one key each, and escaping those keys once more renders one file name
// each rather than the same one twice.
//
// A key that renders past nameMaxLen is named after its SHA-256 digest instead:
// the pair such a document bills is read off run.json, which names the cloud and
// the project id beside every file.
//
// The roadmap writes this file as statement-{project}.json. External project
// ids are unique per cloud only, which is why a statement is stored under a key
// of both halves, and why the file is named after that key rather than after
// the project alone (author's decision of 2026-08-24).
func DocumentFileName(kind, key string) string {
	name := documentPrefix(kind) + url.PathEscape(key) + ".json"
	if len(name) <= nameMaxLen {
		return name
	}
	// A key no file system holds a name for is named after its digest instead.
	return digestFileName(kind, key)
}

// documentPrefix is the prefix the documents of a run's kind carry.
func documentPrefix(kind string) string {
	if kind == runs.KindCorrection {
		return creditNotePrefix
	}
	return statementPrefix
}

// digestFileName names a document after the SHA-256 of its key: the fallback
// for the two keys no escaped name serves, the one that renders past
// nameMaxLen and the one whose name a directory already holds under a different
// case. run.json carries the cloud and the project id beside every file, so the
// pair a digest-named document bills is read off the index rather than off the
// name, and one project id no longer takes the export of the whole month with
// it.
func digestFileName(kind, key string) string {
	return fmt.Sprintf("%s%x.json", documentPrefix(kind), sha256.Sum256([]byte(key)))
}

// runDocument is run.json. The field order is the order it is marshalled in.
// Nothing here records when the export ran: exporting one finalized run twice
// yields the same bytes, and a timestamp of the writing would be the one value
// that does not.
type runDocument struct {
	RunID string `json:"run_id"`
	Kind  string `json:"kind"`
	// The three nullable values are pointers, so what the run row carries as
	// NULL renders as null rather than as an empty string.
	CorrectsRunID  *string          `json:"corrects_run_id"`
	PeriodFrom     string           `json:"period_from"`
	PeriodTo       string           `json:"period_to"`
	Status         string           `json:"status"`
	PricingVersion *string          `json:"pricing_version"`
	Clouds         []string         `json:"clouds"`
	StartedAt      string           `json:"started_at"`
	CompletedAt    *string          `json:"completed_at"`
	Stats          json.RawMessage  `json:"stats"`
	Statements     []statementEntry `json:"statements"`
}

// statementEntry is one document in the index: the file it was written to, the
// pair it bills, and the total it carries. The two halves of the key are
// unescaped, so a reader of the index gets the cloud and the project id as the
// registry holds them.
type statementEntry struct {
	File      string       `json:"file"`
	Cloud     string       `json:"cloud"`
	ProjectID string       `json:"project_id"`
	Total     money.Amount `json:"total"`
	Currency  string       `json:"currency"`
}

// Export writes the run's documents and its index into Dir. Everything is
// rendered before the directory is touched, so a statement whose key or whose
// stored document is refused leaves no directory and no file behind, and a
// write that fails takes the documents before it with it: an export that
// reports an error wrote nothing, rather than every document up to the bad one.
//
// run.json is written last, after every document it names is on stable storage,
// so a reader that picks the index up finds every file it points at, on a node
// that came back from a power loss as well as on one that did not.
//
// The context is not read. What is left is writing a few files, which is not
// cancelable, and stopping halfway through would leave the directory holding
// part of an export.
func (j JSONFiles) Export(_ context.Context, run Run) error {
	index := runDocument{
		RunID:      run.ID.String(),
		Kind:       run.Kind,
		PeriodFrom: instant(run.PeriodFrom),
		PeriodTo:   instant(run.PeriodTo),
		Status:     run.Status,
		Clouds:     run.Clouds,
		StartedAt:  instant(run.StartedAt),
		Stats:      run.Stats,
		// Non-nil, so a run that billed nobody renders an empty list rather than
		// a null: it has no statements, and a null would read as an export that
		// does not say.
		Statements: make([]statementEntry, 0, len(run.Statements)),
	}
	if run.CorrectsRunID != uuid.Nil {
		corrects := run.CorrectsRunID.String()
		index.CorrectsRunID = &corrects
	}
	if run.PricingVersion != "" {
		version := run.PricingVersion
		index.PricingVersion = &version
	}
	if !run.CompletedAt.IsZero() {
		completed := instant(run.CompletedAt)
		index.CompletedAt = &completed
	}

	documents := make(map[string][]byte, len(run.Statements))
	// The names the run renders, folded to lower case. A case-insensitive file
	// system (APFS, SMB, NTFS) resolves two names that differ only in case to
	// one file, so the second rename would replace the first project's document
	// while run.json went on naming both, and an importer opening either name
	// would read one project's invoice under the other's. The second of such a
	// pair is named after its digest instead, which no fold collides with: a
	// project id is free text, so a pair that differs in case alone is valid
	// input rather than a corrupt one, and it is one file name that gives way
	// rather than the month's export.
	folded := make(map[string]bool, len(run.Statements))
	for _, statement := range run.Statements {
		cloud, projectID, err := statements.ParseKey(statement.Key)
		if err != nil {
			return fmt.Errorf("the statement %s of run %s: %w", statement.Key, run.ID, err)
		}
		document, err := renderDocument(run, statement)
		if err != nil {
			return err
		}
		name := DocumentFileName(run.Kind, statement.Key)
		if folded[strings.ToLower(name)] {
			name = digestFileName(run.Kind, statement.Key)
		}
		folded[strings.ToLower(name)] = true
		documents[name] = document
		index.Statements = append(index.Statements, statementEntry{
			File:      name,
			Cloud:     cloud,
			ProjectID: projectID,
			Total:     money.NewAmount(statement.Total),
			Currency:  statement.Currency,
		})
	}

	body, err := marshal(index)
	if err != nil {
		return fmt.Errorf("rendering %s of run %s: %w", runFileName, run.ID, err)
	}

	if err := prepareDir(j.Dir); err != nil {
		return err
	}
	files := make([]artifact, 0, len(index.Statements))
	for _, entry := range index.Statements {
		files = append(files, artifact{name: entry.File, body: documents[entry.File]})
	}
	return writeIndexedFiles(j.Dir, files, artifact{name: runFileName, body: body})
}

// renderDocument re-renders one stored document. The document is decoded into
// the type its run's kind renders and marshalled again, which is what turns the
// key order JSONB stores an object in back into the field order the concept
// prints it in. Unknown fields are refused rather than dropped: a document the
// engine's own types do not hold every field of is one this version cannot
// render without losing part of it.
func renderDocument(run Run, statement statements.Statement) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(statement.Document))
	decoder.DisallowUnknownFields()

	var document any = &statements.Document{}
	if run.Kind == runs.KindCorrection {
		document = &corrections.CreditNote{}
	}
	if err := decoder.Decode(document); err != nil {
		return nil, fmt.Errorf("decoding the stored document of statement %s of run %s: %w",
			statement.Key, run.ID, err)
	}

	body, err := marshal(document)
	if err != nil {
		return nil, fmt.Errorf("rendering the document of statement %s of run %s: %w",
			statement.Key, run.ID, err)
	}
	return body, nil
}

// marshal renders one artifact the way every JSON file of an export is
// rendered: indented by two spaces and closed by a newline, so a file reads in
// a terminal and ends the way a text file does.
func marshal(document any) ([]byte, error) {
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// instant renders a timestamp the way every export renders one: UTC and RFC
// 3339, at the precision the value carries, so a whole second renders without a
// fraction.
func instant(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
