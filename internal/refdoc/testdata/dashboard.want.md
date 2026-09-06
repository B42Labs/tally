### `dashboard.json`

Title `Tally / Fixture`, uid `tally-fixture`.

| Variable | Multi | Query |
| --- | --- | --- |
| `cloud` | yes | `label_values(tally_current_resources, cloud)` |
| `api_base` | no | `https://api.tally.127-0-0-1.nip.io:8443` |
| `interval` | no | none |

| Panel | Type | Expression |
| --- | --- | --- |
| Ingest | `row` | none |
| Event ingest rate | `timeseries` | `sum by (cloud) (rate(tally_events_ingested_total{cloud=~"$cloud"}[5m]))` |
| Event ingest rate | `timeseries` | `sum by (cloud) (rate(tally_events_rejected_total{cloud=~"$cloud"}[5m]))` |
| Clouds reporting | `stat` | `count(count by (cloud) (tally_current_resources))` |
| How to read this | `text` | none |
