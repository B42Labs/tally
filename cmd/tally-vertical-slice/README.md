# tally-vertical-slice

`tally-vertical-slice` rates one project's instance usage for one calendar
month and prints it as JSON. It reads the Reporting API, folds each instance's
event history with [`internal/core/timeline`](../../internal/core/timeline),
clips the intervals to the month, rates the records against
[`pricing/prototype.yaml`](../../pricing/prototype.yaml), and checks the
metering invariants over what it derived.

## A throwaway prototype

The slice is throwaway on purpose. It is the golden-numbers gate of
[WP 1.15 in the Phase 1 roadmap](../../roadmap/01-phase-1-core-platform-openstack.md),
and its job is to prove the billing chain end to end, once, on figures a person
can check by hand.

Phase 3 replaces it wholesale. WP 3.3 of the
[Phase 3 roadmap](../../roadmap/03-phase-3-metering-rating.md) creates
`internal/engine/metering`, which meters every resource type against the
project graph and versioned pricing models. Only the timeline fold carries
over. Nothing else in this directory does.

That is why the clipping, the rating, and the invariant checks live in this
command instead of in a package under `internal/`. They are the engine's work,
and a prototype's version of them in the core would give code that is meant to
be deleted a home that other packages import. Nothing under `internal/` knows
about this command.

## The egress delta

The slice rates the golden instance `abc-123` at 124.80 EUR for March 2026. The
end-to-end example on the [worked examples page](https://b42labs.github.io/tally/explanation/worked-examples) rates
the same instance at 128.45 EUR. The whole difference is egress: that example
bills 18.0 GB in the first active interval at 1.62 EUR and 22.5 GB in the third
at 2.03 EUR, and 124.80 plus 3.65 is 128.45. The vCPU, RAM, and disk costs of
the two agree interval for interval, and so do the three intervals themselves.

The slice bills no egress because egress is a counter metric rather than a
property of a resource's state timeline: it is summed from usage counters over
the period, which is Phase 3 scope, and WP 1.15 puts it there. The delta is
recorded here rather than approximated. An estimated egress figure would cost
the golden numbers the one property this prototype exists for, that a reader
can recompute them from the events and the price list alone.

## The dev-cluster drill

The drill reproduces the golden numbers against a dev cluster: over TLS through
the Gateway, against the Reporting API the dev overlay deploys, with
credentials issued by the admin CLI. Run the steps in order, in one shell, from
the repository root. The tokens are shell variables the later steps read.

### What the drill needs

Docker and kind on the host, and a dev cluster that is up:

```sh
make up
make migrate
```

`make up` applies the migration chain as its last step. `make migrate` is what
a cluster that was already running when a migration landed needs.

### Trust the dev CA

The Gateway serves a certificate from the cluster's own CA, which the host does
not know. Write it next to the repository; curl and the slice both read it from
there:

```sh
make -s ca > tally-ca.crt
```

### Issue the two credentials

The admin CLI works on the reporting database directly and reads its URL from
`TALLY_REPORTING_DB_URL`. The dev value is the Makefile's `TALLY_DEV_DB_URL`,
which reaches TimescaleDB through the Gateway's TCP listener. A new token is
printed once, alone on stdout, so a command substitution captures it:

```sh
INGEST_TOKEN="$(TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable' \
  go run ./cmd/tally-reporting-admin create-ingest-credential --platform openstack --cloud os-prod-eu1)"
export TALLY_SLICE_TOKEN="$(TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable' \
  go run ./cmd/tally-reporting-admin create-api-token --role read_all)"
```

The ingest credential is scoped to the platform and cloud the batch below
claims, which is what the ingest guard holds the batch against. The query token
is `read_all` because a `project` token is scoped by project registry ids, and
the drill would have to create registry rows before it could name one.
`TALLY_SLICE_TOKEN` is exported because the slice reads its token from the
environment rather than from a flag, which keeps the credential out of the
process's argv.

### Ingest the golden history

One call submits all five events as a single JSON array. Two instances share
the month: `abc-123` is created in February, powered off on 03-11 and on again
on 03-21, and `def-456` is created on 03-01 and resized on 03-16.

```sh
curl --cacert tally-ca.crt -X POST \
  'https://api.tally.127-0-0-1.nip.io:8443/api/v1/events' \
  -H "Authorization: Bearer $INGEST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '[
  {
    "event_id": "vs-abc-123-create",
    "timestamp": "2026-02-10T08:00:00Z",
    "event_type": "compute.instance.create.end",
    "platform": "openstack",
    "cloud": "os-prod-eu1",
    "resource_type": "instance",
    "resource_id": "abc-123",
    "project_id": "proj-456",
    "payload": {
      "state": "active",
      "size": {"vcpus": 4, "ram_gb": 8, "disk_gb": 80, "flavor": "m1.large"}
    }
  },
  {
    "event_id": "vs-abc-123-power-off",
    "timestamp": "2026-03-11T00:00:00Z",
    "event_type": "compute.instance.power_off",
    "platform": "openstack",
    "cloud": "os-prod-eu1",
    "resource_type": "instance",
    "resource_id": "abc-123",
    "project_id": "proj-456",
    "payload": {"state": "shutoff"}
  },
  {
    "event_id": "vs-abc-123-power-on",
    "timestamp": "2026-03-21T00:00:00Z",
    "event_type": "compute.instance.power_on",
    "platform": "openstack",
    "cloud": "os-prod-eu1",
    "resource_type": "instance",
    "resource_id": "abc-123",
    "project_id": "proj-456",
    "payload": {"state": "active"}
  },
  {
    "event_id": "vs-def-456-create",
    "timestamp": "2026-03-01T00:00:00Z",
    "event_type": "compute.instance.create.end",
    "platform": "openstack",
    "cloud": "os-prod-eu1",
    "resource_type": "instance",
    "resource_id": "def-456",
    "project_id": "proj-456",
    "payload": {
      "state": "active",
      "size": {"vcpus": 2, "ram_gb": 4, "disk_gb": 40, "flavor": "m1.small"}
    }
  },
  {
    "event_id": "vs-def-456-resize",
    "timestamp": "2026-03-16T00:00:00Z",
    "event_type": "compute.instance.resize.end",
    "platform": "openstack",
    "cloud": "os-prod-eu1",
    "resource_type": "instance",
    "resource_id": "def-456",
    "project_id": "proj-456",
    "payload": {
      "state": "active",
      "size": {"vcpus": 4, "ram_gb": 8, "disk_gb": 80, "flavor": "m1.large"}
    }
  }
]'
```

The answer is 200 with:

```json
{"accepted":5,"duplicates":0,"rejected":[]}
```

Repeating the call is safe. The insert carries `ON CONFLICT (event_id,
timestamp) DO NOTHING`
([`queries.sql`](../../internal/reporting/store/queries.sql)), so a second run
answers `{"accepted":0,"duplicates":5,"rejected":[]}` and books nothing twice.

### Rate the month

```sh
go run ./cmd/tally-vertical-slice \
  --cloud os-prod-eu1 --project proj-456 --month 2026-03 \
  --reporting-url https://api.tally.127-0-0-1.nip.io:8443 \
  --pricing pricing/prototype.yaml --ca-file tally-ca.crt
```

The run reads its token from `TALLY_SLICE_TOKEN`, which the credential step
exported. `--reporting-url` is the API's root without the `/api/v1` suffix, and
`--ca-file` replaces the system trust store for the run, which is what reaches
a Gateway whose certificate the host does not verify on its own.

### The document

The run prints this document, and
[`slice_integration_test.go`](slice_integration_test.go) asserts the same
numbers in CI:

```json
{
  "cloud": "os-prod-eu1",
  "project_id": "proj-456",
  "period": {
    "from": "2026-03-01T00:00:00Z",
    "to": "2026-04-01T00:00:00Z"
  },
  "currency": "EUR",
  "resources": [
    {
      "resource_id": "abc-123",
      "warnings": [],
      "violations": [],
      "records": [
        {
          "from": "2026-03-01T00:00:00Z",
          "to": "2026-03-11T00:00:00Z",
          "state": "active",
          "seconds": 864000,
          "minutes": 14400.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 80,
              "cost": 19.20
            },
            "ram_gb": {
              "quantity": 8,
              "cost": 9.60
            },
            "vcpus": {
              "quantity": 4,
              "cost": 19.20
            }
          },
          "subtotal": 48.00
        },
        {
          "from": "2026-03-11T00:00:00Z",
          "to": "2026-03-21T00:00:00Z",
          "state": "shutoff",
          "seconds": 864000,
          "minutes": 14400.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 80,
              "cost": 9.60
            },
            "ram_gb": {
              "quantity": 8,
              "cost": 4.80
            },
            "vcpus": {
              "quantity": 4,
              "cost": 9.60
            }
          },
          "subtotal": 24.00
        },
        {
          "from": "2026-03-21T00:00:00Z",
          "to": "2026-04-01T00:00:00Z",
          "state": "active",
          "seconds": 950400,
          "minutes": 15840.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 80,
              "cost": 21.12
            },
            "ram_gb": {
              "quantity": 8,
              "cost": 10.56
            },
            "vcpus": {
              "quantity": 4,
              "cost": 21.12
            }
          },
          "subtotal": 52.80
        }
      ],
      "total": 124.80
    },
    {
      "resource_id": "def-456",
      "warnings": [],
      "violations": [],
      "records": [
        {
          "from": "2026-03-01T00:00:00Z",
          "to": "2026-03-16T00:00:00Z",
          "state": "active",
          "seconds": 1296000,
          "minutes": 21600.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 40,
              "cost": 14.40
            },
            "ram_gb": {
              "quantity": 4,
              "cost": 7.20
            },
            "vcpus": {
              "quantity": 2,
              "cost": 14.40
            }
          },
          "subtotal": 36.00
        },
        {
          "from": "2026-03-16T00:00:00Z",
          "to": "2026-04-01T00:00:00Z",
          "state": "active",
          "seconds": 1382400,
          "minutes": 23040.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 80,
              "cost": 30.72
            },
            "ram_gb": {
              "quantity": 8,
              "cost": 15.36
            },
            "vcpus": {
              "quantity": 4,
              "cost": 30.72
            }
          },
          "subtotal": 76.80
        }
      ],
      "total": 112.80
    }
  ],
  "total": 237.60
}
```

Both `warnings` arrays are empty, and so are both `violations` arrays: the
golden history folds without a gap, and the records tile exactly the part of
March each instance lived through.

The exit status is 0 for a clean run. A run whose records break a metering
invariant prints the document first and exits 1, with the breaches in the
resource's `violations` array: the numbers are what the run is for, so they
stay readable while the status still reports the failure. Any other error exits
1 without a document.

### Why the drill runs outside CI

CI runs `go test ./...` on a runner that has Docker and no kind cluster
([`ci.yaml`](../../.github/workflows/ci.yaml)), so the drill is not part of it.
`slice_integration_test.go` proves the same numbers there instead: it assembles
the real router, ingest pipeline, and authenticator over a migrated TimescaleDB
in a container, submits the same five events, and holds the printed document
against the concept's figures value by value. The drill adds what that test
leaves out: the Gateway, TLS against the dev CA, and credentials issued by the
admin CLI. The two halves together are WP 1.15's verification.
