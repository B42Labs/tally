| Name | Value | Meaning |
| --- | --- | --- |
| `TypeValidation` | `urn:tally:test:validation` | TypeValidation marks a request the contract rejects. |
| `TypeInternal` | `urn:tally:test:internal` | TypeInternal marks a failure the caller cannot do anything about. |
| `SourceCollector` | `collector` | SourceCollector marks an event a provider-side collector pushed. |
| `identifierMaxLen` | `512` | identifierMaxLen bounds the fields that identify a resource. They are indexed columns, and a value past the btree limit fails the insert. |
