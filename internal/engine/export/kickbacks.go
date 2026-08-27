package export

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/adjustment"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// The two files the settlement is written to: the document by the JSON writer
// and the table by the CSV writer.
const (
	kickbacksJSONFileName = "kickbacks.json"
	kickbacksCSVFileName  = "kickbacks.csv"
)

// kickbacksHeader is the column order of kickbacks.csv. The fourteen columns
// are aligned with ratedHeader and deltasHeader rather than the ten the
// roadmap's WP 5.4 lists (author's decision of 2026-08-27, named here per
// guardrail 10 of roadmap/00-conventions.md): adjustment_records.project_id
// holds the statement key cloud/project, and every other artifact renders that
// pair as two members, because external project ids are unique per cloud only.
// kind and corrects_run_id say whether a row is a difference a correction owes
// rather than a payout of the month, and relation_id is what the auditability
// drill walks back to the registry.
var kickbacksHeader = []string{
	"run_id", "kind", "corrects_run_id", "period_from", "period_to",
	"beneficiary", "cloud", "project_id", "relation_id", "scope",
	"rate", "base", "amount", "currency",
}

// Kickback is one kickback a run settles for a partner. For a regular run it
// is one adjustment_records row of type kickback. For a correction it is one
// non-zero difference between the correction's kickback records and those of
// the run it corrects, under the key the credit note diffs by: Base and
// Amount are then new minus old, negative where usage was corrected down.
type Kickback struct {
	Beneficiary string // the partner's external id, adjustment_records.beneficiary
	Currency    string
	// StatementKey is adjustment_records.project_id as stored, the
	// statements.Key rendering; Cloud and ProjectID are its two halves.
	StatementKey     string
	Cloud, ProjectID string
	RelationID       uuid.UUID
	Scope            string
	Rate             decimal.Decimal
	Base, Amount     decimal.Decimal
}

// kickbackKey is what the two sides of a correction's kickbacks are compared
// under: the statement the kickback was applied to, the relation it came from,
// and the scope and the rate of the element. It is corrections.AdjustmentKey
// without the type, which is the kickback type on every row that gets here.
//
// Rate is the six-place text money.RatePlaces renders, the scale the
// adjustment schema admits, because a decimal is not a map key.
type kickbackKey struct {
	StatementKey string
	RelationID   uuid.UUID
	Scope, Rate  string
}

// loadKickbacks reads what the run settles for its partners. A regular run's
// kickbacks are its adjustment records of type kickback. A correction's are
// the differences to the kickbacks of the run it corrects, and both sides are
// read inside the snapshot Load opened, so a difference is taken between two
// states of one instant.
func loadKickbacks(ctx context.Context, q *sqlcgen.Queries, run Run) ([]Kickback, error) {
	rows, err := q.ListAdjustmentRecords(ctx, pgtype.UUID{Bytes: run.ID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("reading the adjustment records of run %s: %w", run.ID, err)
	}
	current, err := kickbacksOf(rows, run.ID)
	if err != nil {
		return nil, err
	}
	if run.Kind != runs.KindCorrection {
		sortKickbacks(current)
		return current, nil
	}

	// runs.corrects_run_id carries no CHECK tying it to the correction kind, the
	// way billing_periods and adjustment_records tie their columns, so a
	// correction row that names no corrected run is one the schema admits. It is
	// refused rather than diffed against the empty baseline the nil uuid reads
	// back: that baseline reports the correction's whole month as differences,
	// and a partner already settled from the regular run's report would be owed
	// the same amounts a second time.
	if run.CorrectsRunID == uuid.Nil {
		return nil, fmt.Errorf(
			"the correction run %s names no corrected run, and its kickbacks are the differences to one",
			run.ID)
	}

	rows, err = q.ListAdjustmentRecords(ctx, pgtype.UUID{Bytes: run.CorrectsRunID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("reading the adjustment records of the corrected run %s: %w",
			run.CorrectsRunID, err)
	}
	old, err := kickbacksOf(rows, run.CorrectsRunID)
	if err != nil {
		return nil, err
	}
	return DiffKickbacks(old, current), nil
}

// kickbacksOf keeps the kickbacks of one run's adjustment records. The listing
// hands back every type a run applied, and a surcharge or a discount is what a
// project was billed rather than what a partner is settled with, so the other
// three types are skipped here.
//
// A stored rate, base or amount that is not a number is refused by naming the
// row it was read from, the way loadRated refuses a rated amount: a payout
// short of a value, or one holding a zero where a number was meant, is one
// nobody can tell from a correct one.
func kickbacksOf(rows []sqlcgen.AdjustmentRecord, runID uuid.UUID) ([]Kickback, error) {
	result := make([]Kickback, 0, len(rows))
	for _, row := range rows {
		if row.Type != adjustment.TypeKickback {
			continue
		}
		relationID := uuid.UUID(row.RelationID.Bytes)
		// The CHECK of migration 0002 ties a non-null beneficiary to the kickback
		// type, so a kickback row without one does not reach this. It is checked
		// rather than rendered under an empty name: a payout nobody is named for
		// is one no partner can be settled from.
		if !row.Beneficiary.Valid {
			return nil, fmt.Errorf("the kickback of relation %s on %s of run %s names no beneficiary",
				relationID, row.ProjectID, runID)
		}
		rate, rateNumber := amountOf(row.Rate)
		base, baseNumber := amountOf(row.Base)
		amount, amountNumber := amountOf(row.Amount)
		if !rateNumber || !baseNumber || !amountNumber {
			return nil, fmt.Errorf("the kickback of relation %s on %s of run %s is not a number",
				relationID, row.ProjectID, runID)
		}
		cloud, projectID, err := statements.ParseKey(row.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("the kickback of relation %s of run %s: %w", relationID, runID, err)
		}
		result = append(result, Kickback{
			Beneficiary:  row.Beneficiary.String,
			Currency:     row.Currency,
			StatementKey: row.ProjectID,
			Cloud:        cloud,
			ProjectID:    projectID,
			RelationID:   relationID,
			Scope:        row.Scope,
			Rate:         rate,
			Base:         base,
			Amount:       amount,
		})
	}
	return result, nil
}

// DiffKickbacks is what changed between the kickbacks of a correction and
// those of the run it corrects: one Kickback per key they disagree on.
//
// The key is the statement, the relation, the scope and the rate, which is
// what corrections.DiffAdjustments diffs a credit note by. Base and Amount are
// summed per key on each side first, so two elements of one relation under one
// key are one kickback there, and the result carries current minus old. A key
// one side does not hold reads as zero on that side: a kickback the correction
// no longer settles is taken back whole, and one it settles for the first time
// is added whole.
//
// A key whose amount difference is zero is left out, whatever its base
// difference did, because DiffAdjustments drops it too and the report follows
// the credit note. Beneficiary, Currency, Cloud, ProjectID and Rate come from
// the current side where it holds the key and from the old side otherwise,
// while the statement, the relation and the scope are the key's. The result is
// sorted the way sortKickbacks sorts a run's own kickbacks, and two sides that
// settle the same, two empty ones included, yield nil rather than an empty
// slice.
func DiffKickbacks(old, current []Kickback) []Kickback {
	oldSums, currentSums := sumKickbacks(old), sumKickbacks(current)

	var result []Kickback
	for key, kickback := range currentSums {
		// A key old does not hold reads as the zero decimal, which is what the
		// correction settling the kickback for the first time is diffed against.
		result = appendKickbackDifference(result, kickback,
			kickback.Base.Sub(oldSums[key].Base), kickback.Amount.Sub(oldSums[key].Amount))
	}
	for key, kickback := range oldSums {
		if _, held := currentSums[key]; held {
			continue
		}
		result = appendKickbackDifference(result, kickback, kickback.Base.Neg(), kickback.Amount.Neg())
	}

	sortKickbacks(result)
	return result
}

// sumKickbacks adds up what one side of the diff settled per key. One relation
// contributes at most one element per type and scope, so two kickbacks under
// one key are two records of the same relation over the same statement, and
// what the diff compares is their sum, the way baselineAdjustments in
// internal/engine/runs/correct.go sums the adjustment records of a corrected
// run. The first kickback under a key carries the names and the rate the sum
// is reported with; every later one adds its base and its amount.
func sumKickbacks(kickbacks []Kickback) map[kickbackKey]Kickback {
	sums := make(map[kickbackKey]Kickback, len(kickbacks))
	for _, kickback := range kickbacks {
		key := kickbackKey{
			StatementKey: kickback.StatementKey,
			RelationID:   kickback.RelationID,
			Scope:        kickback.Scope,
			Rate:         kickback.Rate.StringFixed(money.RatePlaces),
		}
		sum, held := sums[key]
		if !held {
			sum = kickback
			sum.Base, sum.Amount = decimal.Zero, decimal.Zero
		}
		sum.Base = sum.Base.Add(kickback.Base)
		sum.Amount = sum.Amount.Add(kickback.Amount)
		sums[key] = sum
	}
	return sums
}

// appendKickbackDifference keeps one difference where there is one. The names
// and the rate come from the side the key was read off, and the two amounts
// replace what that side settled. A zero amount difference is dropped whatever
// the base did, the way appendAdjustmentDelta drops it.
func appendKickbackDifference(result []Kickback, kickback Kickback, base, amount decimal.Decimal) []Kickback {
	if amount.IsZero() {
		return result
	}
	kickback.Base, kickback.Amount = base, amount
	return append(result, kickback)
}

// sortKickbacks puts the kickbacks in the order a settlement is read in: the
// partner first, because what one partner is owed is one block, and then the
// statement, the relation and the element under it. The order is total over
// what a run can store, so exporting one run twice yields one order.
func sortKickbacks(kickbacks []Kickback) {
	slices.SortFunc(kickbacks, func(a, b Kickback) int {
		return cmp.Or(
			cmp.Compare(a.Beneficiary, b.Beneficiary),
			cmp.Compare(a.Currency, b.Currency),
			cmp.Compare(a.StatementKey, b.StatementKey),
			// Over the sixteen bytes rather than over the text they render as,
			// which is the order Postgres sorts a uuid in.
			bytes.Compare(a.RelationID[:], b.RelationID[:]),
			cmp.Compare(a.Scope, b.Scope),
			a.Rate.Cmp(b.Rate),
			a.Amount.Cmp(b.Amount),
			a.Base.Cmp(b.Base),
		)
	})
}

// kickbacksDocument is kickbacks.json: the run the settlement belongs to and
// one entry per partner it owes. The field order is the order it is marshalled
// in, and nothing here records when the report ran, for the reason runDocument
// gives.
//
// A correction's entries carry the differences to the run it corrects under the
// same shape a regular run's payouts take. Kind and CorrectsRunID are what say
// which of the two a document holds, so a partner reads a month and the
// correction of it with one reader rather than with two.
type kickbacksDocument struct {
	RunID string `json:"run_id"`
	Kind  string `json:"kind"`
	// A pointer, so a regular run renders null the way runDocument renders the
	// run it corrects.
	CorrectsRunID *string `json:"corrects_run_id"`
	PeriodFrom    string  `json:"period_from"`
	PeriodTo      string  `json:"period_to"`
	// Never nil, so a run that owes nobody renders an empty list rather than a
	// null: it settles nothing, and a null would read as a report that does not
	// say.
	Beneficiaries []beneficiaryEntry `json:"beneficiaries"`
}

// beneficiaryEntry is what one partner is settled with in one currency: the
// total it is paid, the number of projects that total came off, and the rows it
// was summed from. Two currencies under one partner are two entries, because a
// sum over two of them is not a payout anybody can make.
type beneficiaryEntry struct {
	Beneficiary   string          `json:"beneficiary"`
	Currency      string          `json:"currency"`
	KickbackTotal money.Amount    `json:"kickback_total"`
	Projects      int             `json:"projects"`
	Breakdown     []kickbackEntry `json:"breakdown"`
}

// kickbackEntry is one settled kickback under a partner: the statement it came
// off, the relation and the element it was computed from, and the three numbers
// the partner reconciles the payout against.
type kickbackEntry struct {
	Cloud string `json:"cloud"`
	// Two members for the reason json.go gives for the index entries: external
	// project ids are unique per cloud only.
	ProjectID  string       `json:"project_id"`
	RelationID string       `json:"relation_id"`
	Scope      string       `json:"scope"`
	Rate       money.Rate   `json:"rate"`
	Base       money.Amount `json:"base"`
	Amount     money.Amount `json:"amount"`
}

// KickbacksJSON renders the partner-facing settlement document of a run: per
// beneficiary and currency what the partner is owed, over how many projects,
// and the kickbacks that total was summed from.
func KickbacksJSON(run Run) ([]byte, error) {
	// Sorted here rather than taken as it comes: the bytes are a function of the
	// set the run settles rather than of the order it was handed over in. The
	// copy leaves the caller's slice as it is.
	kickbacks := slices.Clone(run.Kickbacks)
	sortKickbacks(kickbacks)

	document := kickbacksDocument{
		RunID:         run.ID.String(),
		Kind:          run.Kind,
		PeriodFrom:    instant(run.PeriodFrom),
		PeriodTo:      instant(run.PeriodTo),
		Beneficiaries: []beneficiaryEntry{},
	}
	if run.CorrectsRunID != uuid.Nil {
		corrects := run.CorrectsRunID.String()
		document.CorrectsRunID = &corrects
	}

	for i, kickback := range kickbacks {
		// The order sorts by the partner and the currency first, so a group ends
		// where either of them changes.
		opened := i == 0 || kickback.Beneficiary != kickbacks[i-1].Beneficiary ||
			kickback.Currency != kickbacks[i-1].Currency
		if opened {
			document.Beneficiaries = append(document.Beneficiaries, beneficiaryEntry{
				Beneficiary: kickback.Beneficiary,
				Currency:    kickback.Currency,
			})
		}

		entry := &document.Beneficiaries[len(document.Beneficiaries)-1]
		// The statement key orders inside a group, so a project is a further one
		// where it differs from the kickback before it.
		if opened || kickback.StatementKey != kickbacks[i-1].StatementKey {
			entry.Projects++
		}
		// The total is the sum of what the rows below it carry, which are amounts
		// the rating rounded already: summing them is what keeps the payout equal
		// to the lines the partner reconciles it against.
		entry.KickbackTotal = money.NewAmount(entry.KickbackTotal.Add(kickback.Amount))
		entry.Breakdown = append(entry.Breakdown, kickbackEntry{
			Cloud:      kickback.Cloud,
			ProjectID:  kickback.ProjectID,
			RelationID: kickback.RelationID.String(),
			Scope:      kickback.Scope,
			Rate:       money.NewRate(kickback.Rate),
			Base:       money.NewAmount(kickback.Base),
			Amount:     money.NewAmount(kickback.Amount),
		})
	}

	body, err := marshal(document)
	if err != nil {
		return nil, fmt.Errorf("rendering %s of run %s: %w", kickbacksJSONFileName, run.ID, err)
	}
	return body, nil
}

// KickbacksCSV renders kickbacks.csv: the header and one row per kickback, in
// the order the settlement is read in. Every row carries the run, its kind and
// its period, the way a row of rated.csv does, so a row says which run and
// which month it belongs to on its own.
//
// A run that settles nothing renders the header alone: an empty table says the
// run owes no partner, and a missing file says nothing at all.
func KickbacksCSV(run Run) ([]byte, error) {
	kickbacks := slices.Clone(run.Kickbacks)
	sortKickbacks(kickbacks)

	corrects := correctsOf(run)
	from, to := instant(run.PeriodFrom), instant(run.PeriodTo)

	rows := [][]string{kickbacksHeader}
	for _, kickback := range kickbacks {
		rows = append(rows, []string{
			run.ID.String(), run.Kind, corrects, from, to,
			cell(kickback.Beneficiary), cell(kickback.Cloud), cell(kickback.ProjectID),
			kickback.RelationID.String(), cell(kickback.Scope),
			// The three numbers do not go through cell: a leading minus is the sign
			// of what a correction takes back rather than a formula.
			kickback.Rate.StringFixed(money.RatePlaces),
			kickback.Base.StringFixed(money.AmountPlaces),
			kickback.Amount.StringFixed(money.AmountPlaces),
			kickback.Currency,
		})
	}
	return table(run, kickbacksCSVFileName, rows)
}

// ErrNoRunForPeriod is what PeriodRun returns for a billing period no
// completed or finalized regular run bills.
var ErrNoRunForPeriod = errors.New("the billing period has no run to report")

// PeriodRun is the run a period's kickbacks are reported from when the caller
// names the month alone: the regular run that closed it, or the completed
// regular run of a month that is still open.
//
// A finalized correction of the month is never what the month alone resolves
// to. What a partner is settled for a month is what the regular run of that
// month settled, and a correction carries the differences on top of that
// settlement, which are reported by naming the correction with --run.
//
// The period and the run are read under one REPEATABLE READ snapshot, because
// closing a month moves its regular run out of the status the second read
// filters on: finalize commits FinalizeRun and FinalizeBillingPeriod together,
// so a month closed between two separate reads would be read as still open and
// then leave no completed regular run to find. The month that was just closed
// would be refused as one that was never billed, and the operator would be sent
// to start a regular run over a period the engine refuses one for. The
// transaction is read-only and ended by the rollback below: resolving a run
// writes nothing.
func PeriodRun(ctx context.Context, pool *pgxpool.Pool, periodFrom time.Time) (uuid.UUID, error) {
	month := period.Format(periodFrom)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return uuid.Nil, fmt.Errorf("reading the run of %s: %w", month, err)
	}
	// The rollback that ends the snapshot, on a context no cancellation reaches,
	// the way Load ends its own.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	q := sqlcgen.New(tx)
	from := pgtype.Timestamptz{Time: periodFrom, Valid: true}

	row, err := q.GetBillingPeriod(ctx, from)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf(
				"%w: %s has no billing period, and tally-engine run --period %s produces one",
				ErrNoRunForPeriod, month, month)
		}
		return uuid.Nil, fmt.Errorf("reading the billing period %s: %w", month, err)
	}
	// The CHECK of migration 0001 ties the name to the status, so a finalized
	// period names a run. That run is the regular one that closed the month:
	// finalizing a correction of it leaves the name where it is.
	if row.Status == statusFinalized {
		return uuid.UUID(row.FinalizedRunID.Bytes), nil
	}

	id, err := q.LatestCompletedRegularRun(ctx, from)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf(
				"%w: %s has no completed run, and tally-engine run --period %s produces one",
				ErrNoRunForPeriod, month, month)
		}
		return uuid.Nil, fmt.Errorf("reading the completed run of %s: %w", month, err)
	}
	return uuid.UUID(id.Bytes), nil
}
