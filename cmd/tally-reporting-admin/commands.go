package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"

	"github.com/b42labs/tally/internal/reporting/audit"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// The two credential tables. A table name is the audit entry's object type and,
// with the verb appended, its action.
const (
	ingestCredentials = "ingest_credentials"
	apiTokens         = "api_tokens"
)

// newMigrateCmd builds the migrate subcommand.
func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply the pending migrations of the reporting database",
		Long: "Apply the pending migrations of the reporting database.\n\n" +
			"The API server runs no DDL, so this is what brings a database to the schema the server expects.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			applied, err := store.Migrate(cmd.Context(), cfg.DBURL)
			if err != nil {
				return err
			}
			if len(applied) == 0 {
				return write(cmd.OutOrStdout(), "nothing to apply")
			}

			lines := make([]string, 0, len(applied))
			for _, version := range applied {
				lines = append(lines, fmt.Sprintf("applied migration %d", version))
			}
			return write(cmd.OutOrStdout(), lines...)
		},
	}
}

// newMigrateStatusCmd builds the migrate-status subcommand.
func newMigrateStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate-status",
		Short: "Report which migrations the database carries",
		Long: "Report which migrations the database carries.\n\n" +
			"It answers which schema a database is on before code that assumes a newer one is deployed against it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			status, err := store.MigrationStatus(cmd.Context(), cfg.DBURL)
			if err != nil {
				return err
			}

			lines := make([]string, 0, len(status))
			for _, migration := range status {
				state := "pending"
				if migration.Applied {
					state = "applied"
				}
				lines = append(lines, fmt.Sprintf("migration %d %s", migration.Version, state))
			}
			return write(cmd.OutOrStdout(), lines...)
		},
	}
}

// newMigrateDownToCmd builds the migrate-down-to subcommand.
func newMigrateDownToCmd() *cobra.Command {
	var confirmed bool

	cmd := &cobra.Command{
		Use:   "migrate-down-to <version>",
		Short: "Roll the database back to a migration version",
		Long: "Roll the database back to a migration version.\n\n" +
			"Every migration above the given version is undone, which drops the data its tables hold. " +
			"Passing 0 leaves an empty schema. The chain ships these down migrations, so this is what runs them.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("version: %q is not a number: %w", args[0], err)
			}
			if version < 0 {
				return fmt.Errorf("version: %d must not be negative", version)
			}
			// Dropping tables is not something to do because a command line was
			// half-typed, so the intent is stated separately from the target.
			if !confirmed {
				return errors.New("--yes: rolling back drops the data of every migration above the target")
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			rolledBack, err := store.MigrateDownTo(cmd.Context(), cfg.DBURL, version)
			if err != nil {
				return err
			}
			if len(rolledBack) == 0 {
				return write(cmd.OutOrStdout(), "nothing to roll back")
			}

			lines := make([]string, 0, len(rolledBack))
			for _, applied := range rolledBack {
				lines = append(lines, fmt.Sprintf("rolled back migration %d", applied))
			}
			return write(cmd.OutOrStdout(), lines...)
		},
	}

	cmd.Flags().BoolVar(&confirmed, "yes", false, "confirm that the data of the rolled-back migrations may be dropped")
	return cmd
}

// newCreateIngestCredentialCmd builds the create-ingest-credential subcommand.
func newCreateIngestCredentialCmd() *cobra.Command {
	var platform, cloud, description string

	cmd := &cobra.Command{
		Use:   "create-ingest-credential",
		Short: "Issue an ingest credential for one cloud",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Cobra checks that the required flags were passed, not that they
			// carry a value. A credential for the empty cloud would accept
			// events nobody can attribute.
			if platform == "" {
				return errors.New("--platform: must not be empty")
			}
			if cloud == "" {
				return errors.New("--cloud: must not be empty")
			}

			token, err := auth.GenerateIngestToken()
			if err != nil {
				return err
			}

			db, err := openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()

			var id uuid.UUID
			if err := db.WithTx(cmd.Context(), func(tx pgx.Tx) error {
				q := sqlcgen.New(tx)

				created, err := q.CreateIngestCredential(cmd.Context(), sqlcgen.CreateIngestCredentialParams{
					Platform:    platform,
					Cloud:       cloud,
					TokenHash:   auth.HashToken(token),
					Description: optionalText(description),
				})
				if err != nil {
					return fmt.Errorf("creating the ingest credential: %w", err)
				}
				id = created

				return audit.Log(cmd.Context(), q, audit.Entry{
					Actor:      actor,
					Action:     ingestCredentials + ".create",
					ObjectType: ingestCredentials,
					ObjectID:   id.String(),
					Details:    map[string]any{"platform": platform, "cloud": cloud},
				})
			}); err != nil {
				return err
			}

			return printCredential(cmd, ingestCredentials, id, token)
		},
	}

	cmd.Flags().StringVar(&platform, "platform", "", "platform the credential reports for, openstack for example")
	cmd.Flags().StringVar(&cloud, "cloud", "", "cloud the credential reports for")
	cmd.Flags().StringVar(&description, "description", "", "note kept with the credential, such as who asked for it")
	_ = cmd.MarkFlagRequired("platform")
	_ = cmd.MarkFlagRequired("cloud")
	return cmd
}

// newCreateAPITokenCmd builds the create-api-token subcommand.
func newCreateAPITokenCmd() *cobra.Command {
	var role, description string
	var projectIDs []string

	cmd := &cobra.Command{
		Use:   "create-api-token",
		Short: "Issue a query token for one role",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scope, err := parseScope(role, projectIDs)
			if err != nil {
				return err
			}

			token, err := auth.GenerateAPIToken()
			if err != nil {
				return err
			}

			db, err := openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()

			details := map[string]any{"role": role}
			if len(projectIDs) > 0 {
				details["project_ids"] = projectIDs
			}

			var id uuid.UUID
			if err := db.WithTx(cmd.Context(), func(tx pgx.Tx) error {
				q := sqlcgen.New(tx)

				created, err := q.CreateAPIToken(cmd.Context(), sqlcgen.CreateAPITokenParams{
					TokenHash:   auth.HashToken(token),
					Role:        role,
					ProjectIds:  scope,
					Description: optionalText(description),
				})
				if err != nil {
					return fmt.Errorf("creating the api token: %w", err)
				}
				id = created

				return audit.Log(cmd.Context(), q, audit.Entry{
					Actor:      actor,
					Action:     apiTokens + ".create",
					ObjectType: apiTokens,
					ObjectID:   id.String(),
					Details:    details,
				})
			}); err != nil {
				return err
			}

			return printCredential(cmd, apiTokens, id, token)
		},
	}

	cmd.Flags().StringVar(&role, "role", "", fmt.Sprintf("what the token may do: %s, %s, or %s",
		auth.RoleAdmin, auth.RoleReadAll, auth.RoleProject))
	cmd.Flags().StringArrayVar(&projectIDs, "project-id", nil,
		fmt.Sprintf("registry id of a project a %s token may read, repeatable", auth.RoleProject))
	cmd.Flags().StringVar(&description, "description", "", "note kept with the token, such as who asked for it")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

// parseScope checks --role against --project-id and returns the registry ids
// the new token is scoped to. Only a project token carries a scope: an admin or
// read_all token that named projects would read past them without saying so,
// and a project token that named none would read nothing.
//
// The result is never nil, because project_ids is NOT NULL and pgx sends a nil
// slice as NULL.
func parseScope(role string, projectIDs []string) ([]uuid.UUID, error) {
	switch auth.Role(role) {
	case auth.RoleAdmin, auth.RoleReadAll:
		if len(projectIDs) > 0 {
			return nil, fmt.Errorf("--project-id: only a %s token is scoped to projects", auth.RoleProject)
		}
	case auth.RoleProject:
		if len(projectIDs) == 0 {
			return nil, fmt.Errorf("--project-id: a %s token needs at least one project", auth.RoleProject)
		}
	default:
		return nil, fmt.Errorf("--role: %q must be %s, %s, or %s",
			role, auth.RoleAdmin, auth.RoleReadAll, auth.RoleProject)
	}

	scope := make([]uuid.UUID, 0, len(projectIDs))
	for _, raw := range projectIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("--project-id: %q is not a uuid: %w", raw, err)
		}
		scope = append(scope, id)
	}
	return scope, nil
}

// revocation is what the two revoke subcommands differ in: the table they work
// on and the two statements that reach it.
type revocation struct {
	// use is the command's name and its argument.
	use string
	// short is the one-line description.
	short string
	// objectType is the table, which is also what the audit entry records.
	objectType string
	// lock reads the row for update and reports when it was revoked, if it was.
	lock func(context.Context, *sqlcgen.Queries, uuid.UUID) (pgtype.Timestamptz, error)
	// revoke marks the row revoked.
	revoke func(context.Context, *sqlcgen.Queries, uuid.UUID) error
}

// newRevokeIngestCredentialCmd builds the revoke-ingest-credential subcommand.
func newRevokeIngestCredentialCmd() *cobra.Command {
	return newRevokeCmd(revocation{
		use:        "revoke-ingest-credential <id>",
		short:      "Revoke an ingest credential",
		objectType: ingestCredentials,
		lock: func(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID) (pgtype.Timestamptz, error) {
			row, err := q.GetIngestCredentialForUpdate(ctx, id)
			return row.RevokedAt, err
		},
		revoke: func(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID) error {
			return q.RevokeIngestCredential(ctx, id)
		},
	})
}

// newRevokeAPITokenCmd builds the revoke-api-token subcommand.
func newRevokeAPITokenCmd() *cobra.Command {
	return newRevokeCmd(revocation{
		use:        "revoke-api-token <id>",
		short:      "Revoke a query token",
		objectType: apiTokens,
		lock: func(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID) (pgtype.Timestamptz, error) {
			row, err := q.GetAPITokenForUpdate(ctx, id)
			return row.RevokedAt, err
		},
		revoke: func(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID) error {
			return q.RevokeAPIToken(ctx, id)
		},
	})
}

// newRevokeCmd builds one revoke subcommand. The row is locked before it is
// read, so two operators revoking the same id at once leave one audit row
// rather than two.
func newRevokeCmd(r revocation) *cobra.Command {
	return &cobra.Command{
		Use:   r.use,
		Short: r.short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(args[0])
			if err != nil {
				return fmt.Errorf("id: %q is not a uuid: %w", args[0], err)
			}

			db, err := openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()

			alreadyRevoked := false
			if err := db.WithTx(cmd.Context(), func(tx pgx.Tx) error {
				q := sqlcgen.New(tx)

				revokedAt, err := r.lock(cmd.Context(), q, id)
				if err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return fmt.Errorf("%s %s: not found", r.objectType, id)
					}
					return fmt.Errorf("reading the %s row: %w", r.objectType, err)
				}
				if revokedAt.Valid {
					// Revoking twice is not a failure: what the operator asked
					// for holds. Nothing changes, so nothing is audited.
					alreadyRevoked = true
					return nil
				}

				if err := r.revoke(cmd.Context(), q, id); err != nil {
					return fmt.Errorf("revoking the %s row: %w", r.objectType, err)
				}
				return audit.Log(cmd.Context(), q, audit.Entry{
					Actor:      actor,
					Action:     r.objectType + ".revoke",
					ObjectType: r.objectType,
					ObjectID:   id.String(),
				})
			}); err != nil {
				return err
			}

			if alreadyRevoked {
				return write(cmd.OutOrStdout(), fmt.Sprintf("%s %s: already revoked", r.objectType, id))
			}
			return write(cmd.OutOrStdout(), fmt.Sprintf("revoked %s %s", r.objectType, id))
		},
	}
}

// printCredential hands the raw token to stdout and everything else to stderr.
// The split is what lets a caller redirect the token into a file while still
// reading the notice.
func printCredential(cmd *cobra.Command, objectType string, id uuid.UUID, token string) error {
	if err := write(cmd.OutOrStdout(), token); err != nil {
		return err
	}
	return write(cmd.ErrOrStderr(),
		fmt.Sprintf("created %s %s", objectType, id),
		"the token above is printed this one time: store it now, it will not be shown again")
}

// write puts every line on w. A failed write is reported rather than dropped:
// a token the operator's terminal never received must not leave the process
// with a zero exit status.
func write(w io.Writer, lines ...string) error {
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return fmt.Errorf("writing the output: %w", err)
		}
	}
	return nil
}

// optionalText maps a flag that was left empty to NULL.
func optionalText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
