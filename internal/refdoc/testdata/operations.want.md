### `GET /healthz`

Liveness probe

Reports whether the process should keep running.

No credential.

| Status | Description | Body |
| --- | --- | --- |
| `200` | The service is alive. | none |
| `500` | The request failed. The body says how. | `application/problem+json` |

### `GET /api/v1/items/{cloud}`

List the items of one cloud

Returns the items of the cloud, narrowed by whichever filters the request carries.
One call answers one page. `next_cursor` is what the next call passes as `cursor`.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `cloud` | `path` | yes | string | The installation the items live in. |
| `kind` | `query` | no | `draft`, `final` | Serve only the items of this kind. |
| `since` | `query` | no | string, `date-time` | Serve only the items at or after this instant, the inclusive bound of the window. |
| `cursor` | `query` | no | string | The `next_cursor` of the page before this one. It is opaque: a client passes it back as it received it. |

| Status | Description | Body |
| --- | --- | --- |
| `200` | One page of items. | [ItemList](/reference/api/reporting-api-schemas#itemlist) |
| `400` | The request failed. The body says how. | `application/problem+json` |

### `POST /api/v1/items/{cloud}`

Ingest items

Stores a batch of items. The body is either one item or an array of them.

Security: `ingestToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `cloud` | `path` | yes | string | The installation the items live in. |

The request body is `application/json`, a [Item](/reference/api/reporting-api-schemas#item) or an array of [Item](/reference/api/reporting-api-schemas#item).

| Status | Description | Body |
| --- | --- | --- |
| `200` | The batch was processed. | [ItemList](/reference/api/reporting-api-schemas#itemlist) |
| `413` | The batch carried more items than one call takes. | `application/problem+json` |
