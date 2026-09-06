| Metric | Type | Labels | Help |
| --- | --- | --- | --- |
| `tally_fixture_bounded_total` | counter | `cloud` | Deliveries the fixture bounded the clouds of. |
| `tally_fixture_buffer_depth` | gauge | none | Events waiting in the fixture outbox. |
| `tally_fixture_deliveries_total` | counter | none | Batches the fixture delivered. |
| `tally_fixture_events_total` | counter | `cloud`, `source` | Events the fixture stored. |
| `tally_fixture_resources` | gauge | `state` | Resources the fixture holds, by `<state>` as the projection reports it. |
