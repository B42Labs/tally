---
title: OpenStack collector settings
description: Every environment variable the OpenStack collector reads.
quadrant: reference
audience: operator
---

# OpenStack collector settings

`tally-openstack-collector` takes every setting from the environment, and every
variable Tally defines carries the prefix `TALLY_`. Section 8 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md)
states that rule.

A secret is given inline or as a path to a file. The File-backed column names
the companion variable: the companion is the variable's name plus the suffix
`_FILE`, and a companion that holds a path makes that file's content the value.
One trailing newline is trimmed, which is what Kubernetes writes into a Secret
volume. An empty file is refused, and so is setting a variable and its `_FILE`
companion at the same time. The collector applies the rule from a private copy,
`resolveFileSecret` in
[`internal/providers/openstack/config.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/config.go),
rather than from the shared
[`internal/core/envsecret`](https://github.com/B42Labs/tally/blob/main/internal/core/envsecret/envsecret.go)
the Reporting API and the engine use. Both spell the convention the same way.

`TALLY_LOG_LEVEL` takes `DEBUG`, `INFO`, `WARN` or `ERROR`. The match is exact,
so a lower-case `info` is refused.

## Settings

<!-- refdoc:begin settings -->
| Variable | Type | Default | File-backed | Governs |
| --- | --- | --- | --- | --- |
| `TALLY_LOG_LEVEL` | string | `INFO` | no | LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR. |
| `TALLY_METRICS_ENABLED` | boolean | `true` | no | MetricsEnabled exposes the instrumentation: false makes GET /metrics answer 404. The variable has no OSC infix because roadmap section 8 lists it among the common variables every service reads. |
| `TALLY_OSC_HTTP_PORT` | integer | `8080` | no | HTTPPort is the port the probe and metrics endpoints listen on. The collector serves no API of its own. |
| `TALLY_OSC_AMQP_URL` | string | none | yes (`TALLY_OSC_AMQP_URL_FILE`) | AMQPURL is the broker the notifications are consumed from. It carries the broker password, so it supports the *_FILE convention. |
| `TALLY_OSC_EXCHANGES` | list, comma-separated | `nova,neutron,cinder,glance` | no | Exchanges are the service exchanges the collector binds its queue to. The default covers nova, neutron, cinder and glance; a deployment that renamed them through control_exchange lists its own. Octavia publishes on the exchange octavia, and the default leaves it out. The exchanges are declared passively, so a collector refuses to run while one it lists is missing from the broker, and a default naming octavia would stop every deployment that runs none. A deployment with octavia lists nova,neutron,cinder,glance,octavia. |
| `TALLY_OSC_TOPICS` | list, comma-separated | `notifications.info` | no | Topics are the notification topics bound on each exchange, matching the notification_topics of the services being collected. |
| `TALLY_OSC_CLOUD` | string | none | no | Cloud is the cloud name every emitted event is attributed to. It has no default because a guessed cloud silently books usage to the wrong one. |
| `TALLY_OSC_REPORTING_URL` | string | none | no | ReportingURL is the base URL of the Reporting API the sender posts to. It must be an absolute https URL, because the ingest token travels on it; ReportingInsecure is what allows a plaintext one. |
| `TALLY_OSC_REPORTING_INSECURE` | boolean | `false` | no | ReportingInsecure allows an http Reporting API. It exists for a collector and an API on the same trusted network, and for development; anywhere else it puts the ingest token on the wire in cleartext. |
| `TALLY_OSC_TOKEN` | string | none | yes (`TALLY_OSC_TOKEN_FILE`) | Token authenticates the sender against the Reporting API. Supports the *_FILE convention. |
| `TALLY_OSC_BUFFER_PATH` | string | none | no | BufferPath is the SQLite file backing the outbox. It belongs on a volume that outlives the container: everything consumed but not yet delivered lives there and nowhere else. |
| `TALLY_OSC_BATCH_MAX` | integer | `500` | no | BatchMax is how many buffered events one POST carries, bounded by what the ingest API accepts. |
| `TALLY_OSC_FLUSH_INTERVAL_S` | integer | `5` | no | FlushIntervalSeconds is how long the sender waits before posting a batch that has not filled up. |
| `TALLY_OSC_BUFFER_MAX_EVENTS` | integer | `1000000` | no | BufferMaxEvents is the outbox depth at which the collector stops consuming. The events then wait on the bus instead of being dropped, which is what keeps an unreachable Reporting API from costing usage data. |
| `TALLY_OSC_PREFETCH` | integer | `100` | no | Prefetch is the AMQP QoS bound: how many unacknowledged messages the broker hands out. Acks follow the outbox insert, so this bounds how much work a crash replays. |
| `TALLY_OSC_UNHEALTHY_THRESHOLD_S` | integer | `600` | no | UnhealthyThresholdSeconds is how long readiness may keep failing before liveness fails too and the orchestrator restarts the pod. |
<!-- refdoc:end settings -->

## What is checked

`Load` runs in both modes. It resolves the two file-backed secrets, trims a
trailing slash off `TALLY_OSC_REPORTING_URL`, and then checks:

- `TALLY_LOG_LEVEL` is one of the four levels above.
- `TALLY_OSC_BATCH_MAX` is between 1 and 1000. The upper bound is
  `maxBatchItems` in
  [`internal/providers/openstack/config.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/config.go),
  and it is the longest array `POST /api/v1/events` accepts.
- `TALLY_OSC_FLUSH_INTERVAL_S` is positive.
- `TALLY_OSC_BUFFER_MAX_EVENTS` is positive.
- `TALLY_OSC_PREFETCH` is positive.
- `TALLY_OSC_UNHEALTHY_THRESHOLD_S` is positive.

`ValidateServe` is the collecting mode's startup gate. It adds:

- `TALLY_OSC_AMQP_URL`, `TALLY_OSC_CLOUD`, `TALLY_OSC_REPORTING_URL`,
  `TALLY_OSC_TOKEN` and `TALLY_OSC_BUFFER_PATH` are set.
- `TALLY_OSC_REPORTING_URL` is an absolute `http` or `https` URL with a host
  and with no query and no fragment. The ingest path is appended to the value,
  and appending to a query or a fragment puts that path inside it.
- `TALLY_OSC_REPORTING_URL` uses `https`, unless
  `TALLY_OSC_REPORTING_INSECURE` is `true`. The ingest token travels on every
  flush.

`ValidateDump` is the gate of `--dump`. It asks for `TALLY_OSC_AMQP_URL` and
nothing else: the mode maps nothing and posts nothing.

## Example

[`cmd/tally-openstack-collector/.env.example`](https://github.com/B42Labs/tally/blob/main/cmd/tally-openstack-collector/.env.example)
lists every variable with its default and a comment.
