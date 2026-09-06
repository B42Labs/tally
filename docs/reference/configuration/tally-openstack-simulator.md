---
title: OpenStack simulator settings
description: Every environment variable the OpenStack simulator reads, and which subcommand reads it.
quadrant: reference
audience: operator
---

# OpenStack simulator settings

`tally-openstack-simulator` takes every setting from the environment, and every
variable Tally defines carries the prefix `TALLY_`. Section 8 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md)
states that rule.

A secret is given inline or as a path to a file. The File-backed column names
the companion variable: the companion is the variable's name plus the suffix
`_FILE`, and a companion that holds a path makes that file's content the value.
One trailing newline is trimmed, which is what Kubernetes writes into a Secret
volume. An empty file is refused, and so is setting a variable and its `_FILE`
companion at the same time. The simulator applies the rule from a private copy,
`resolveFileSecret` in
[`internal/providers/openstack/simulator/config.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/simulator/config.go),
rather than from the shared
[`internal/core/envsecret`](https://github.com/B42Labs/tally/blob/main/internal/core/envsecret/envsecret.go)
the Reporting API and the engine use. Both spell the convention the same way.

`TALLY_LOG_LEVEL` takes `DEBUG`, `INFO`, `WARN` or `ERROR`. The match is exact,
so a lower-case `info` is refused.

## Settings

<!-- refdoc:begin settings -->
| Variable | Type | Default | File-backed | Governs |
| --- | --- | --- | --- | --- |
| `TALLY_LOG_LEVEL` | string | `INFO` | no | LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR. Both subcommands read it. |
| `TALLY_SIM_HTTP_ADDR` | string | `127.0.0.1` | no | HTTPAddr is the address the control endpoint binds. It defaults to loopback because the endpoint carries no credential and PUT /clock changes the pace of a run: a simulator on a host beside a control plane would otherwise answer everybody on the management network. A deployment that means to publish the port sets 0.0.0.0, which is what the compose stack does. |
| `TALLY_SIM_HTTP_PORT` | integer | `8080` | no | HTTPPort is the port the control endpoint listens on. It is served only while the simulator publishes, because there is nothing to control once the month is over. |
| `TALLY_SIM_AMQP_URL` | string | none | yes (`TALLY_SIM_AMQP_URL_FILE`) | AMQPURL is the broker the notifications are published to. It carries the broker password, so it supports the *_FILE convention. Empty puts run in file mode, where the month is written out instead of published. |
| `TALLY_SIM_CLOUD` | string | none | no | Cloud is read by run alone: it is the salt of every generated identifier and the cloud of events.jsonl. It has no default because a guessed cloud silently books usage to the wrong one. |
| `TALLY_SIM_REPORTING_URL` | string | none | no | ReportingURL is the Reporting API the projects are registered with. run reads it when --register-projects is on and ignores it otherwise. It must be an absolute https URL, because the api token travels on it; ReportingInsecure is what allows a plaintext one. |
| `TALLY_SIM_REPORTING_INSECURE` | boolean | `false` | no | ReportingInsecure allows an http Reporting API. It exists for a simulator and an API on the same machine, and for development; anywhere else it puts an api token of role admin on the wire in cleartext. |
| `TALLY_SIM_API_TOKEN` | string | none | yes (`TALLY_SIM_API_TOKEN_FILE`) | APIToken is an api token of role admin, which POST /api/v1/projects and POST /api/v1/projects/{id}/relations demand. It supports the *_FILE convention. |
| `TALLY_SIM_GARDEN_CLOUD` | string | none | no | GardenCloud is the cloud the two Gardener projects are registered under. It has no default, for the reason Cloud has none. |
| `TALLY_SIM_OTLP_URL` | string | none | no | OTLPURL is the OTLP/HTTP endpoint the traffic and inventory series of a run are pushed to. Empty is a run without a push. It has to be absolute and carry a host, and to be https unless OTLPInsecure allows a plaintext one, because the Basic password travels on it. run reads it alone. |
| `TALLY_SIM_OTLP_USER` | string | none | no | OTLPUser is the Basic user of the push. The endpoint in front of the collector takes Basic auth, so a URL without a user reaches nothing. |
| `TALLY_SIM_OTLP_PASSWORD` | string | none | yes (`TALLY_SIM_OTLP_PASSWORD_FILE`) | OTLPPassword is the Basic password of the push. It supports the *_FILE convention. |
| `TALLY_SIM_OTLP_INSECURE` | boolean | `false` | no | OTLPInsecure allows an http endpoint. It exists for a simulator and a collector on the same machine, and for development; anywhere else it puts the Basic password on the wire in cleartext. |
| `TALLY_METRICS_ENABLED` | boolean | `true` | no | MetricsEnabled serves the inventory on GET /metrics of the control listener while a run publishes, where it stands in for the OpenStack database exporter a deployment scrapes. False registers no route, and the fake OpenStack API then answers that path with its own 404. A Config that never went through Load carries false, which is why a test that needs the endpoint sets it. The variable has no SIM infix because roadmap section 8 lists it among the common variables every service reads. |
<!-- refdoc:end settings -->

`compare` reads none of them: it takes the three files it compares from its
flags.

`run` reads all of them. `TALLY_SIM_CLOUD` is its alone, and so are
`TALLY_METRICS_ENABLED` and the four `TALLY_SIM_OTLP_` variables.
`TALLY_SIM_REPORTING_URL`, `TALLY_SIM_REPORTING_INSECURE`,
`TALLY_SIM_API_TOKEN` and `TALLY_SIM_GARDEN_CLOUD` are read with
`--register-projects` and ignored otherwise. An empty `TALLY_SIM_AMQP_URL` puts
`run` in file mode, where the month is written out instead of published.

`replay` reads `TALLY_SIM_AMQP_URL`, which it requires, `TALLY_LOG_LEVEL`, and
`TALLY_SIM_HTTP_ADDR` and `TALLY_SIM_HTTP_PORT` for the control endpoint it
serves while it publishes. The recorded notifications carry the cloud, so it
reads none of the rest.

## What is checked

`Load` runs for `run` and for `replay`. It resolves the three file-backed
secrets and checks that `TALLY_LOG_LEVEL` is one of the four levels above.

`ValidateRun` is the gate of `run`. It adds `TALLY_SIM_CLOUD` is set. The
broker stays optional, because `run --out` writes the month to files.

`ValidateReplay` is the gate of `replay`. It adds `TALLY_SIM_AMQP_URL` is set.

`ValidateRegistration` is the gate of `run --register-projects`. It adds:

- `TALLY_SIM_REPORTING_URL` is set.
- `TALLY_SIM_REPORTING_URL` is an absolute `http` or `https` URL with a host
  and with no query and no fragment. The registry route is appended to the
  value.
- `TALLY_SIM_REPORTING_URL` uses `https`, unless
  `TALLY_SIM_REPORTING_INSECURE` is `true`. The api token travels on every
  request and is of role `admin`.
- `TALLY_SIM_API_TOKEN` is set.
- `TALLY_SIM_GARDEN_CLOUD` is set, and differs from `TALLY_SIM_CLOUD`. A cloud
  is one installation of one platform.

`ValidateMetrics` is the gate of the push, checked before `run` dials the
broker. An empty `TALLY_SIM_OTLP_URL` is a run without a push and passes. A
value that is set adds:

- `TALLY_SIM_OTLP_USER` is set.
- `TALLY_SIM_OTLP_PASSWORD` is set.
- `TALLY_SIM_OTLP_URL` is an absolute `http` or `https` URL with a host.
- `TALLY_SIM_OTLP_URL` carries no userinfo, no query and no fragment. The
  credential of a push belongs in the two variables above.
- `TALLY_SIM_OTLP_URL` uses `https`, unless `TALLY_SIM_OTLP_INSECURE` is
  `true`. The Basic password travels on it.

## Example

[`cmd/tally-openstack-simulator/.env.example`](https://github.com/B42Labs/tally/blob/main/cmd/tally-openstack-simulator/.env.example)
lists every variable with its default and a comment.
