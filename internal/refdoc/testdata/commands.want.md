### `tally-fixture`

Meter and rate the fixture

```text
tally-fixture
```

| Subcommand | Purpose |
| --- | --- |
| `export` | Export the statements of a run |
| `migrate-down-to` | Roll the fixture database back to a migration version |
| `pricing` | Work with the pricing catalogs |

This command takes no flags.

### `tally-fixture export`

Export the statements of a run

Export the statements of a run.

A rollup writes one document per group, `rollup-<key>.json` or rollup.csv, summing the statements of its members.

```text
tally-fixture export [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--clouds` | list, comma-separated | none | no | clouds to export, comma-separated |
| `--dry-run` | boolean | `false` | no | report what would be written and write nothing |
| `--factor` | number | `1.5` | no | virtual seconds per wall second |
| `--label` | string, repeatable | none | no | label to stamp on every document |
| `--period` | string | none | yes | billing month to export, as YYYY-MM |
| `--seed` | integer | `1` | no | seed of the month's shape |
| `--wait` | duration | `30s` | no | how long to wait for a consumer |

### `tally-fixture migrate-down-to <version>`

Roll the fixture database back to a migration version

```text
tally-fixture migrate-down-to <version>
```

This command takes no flags.

### `tally-fixture pricing`

Work with the pricing catalogs

```text
tally-fixture pricing
```

| Subcommand | Purpose |
| --- | --- |
| `list` | List the imported pricing catalogs |

This command takes no flags.

### `tally-fixture pricing list`

List the imported pricing catalogs

```text
tally-fixture pricing list
```

This command takes no flags.
