#### `Document`

Document is one project's statement as the export writes it.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `billing_period` | [Period](#period) | always |  |
| `project_id` | string | always |  |
| `line_items` | array of [LineItem](#lineitem) | always | LineItems is what the project is billed for, one entry per resource. |
| `base_cost` | decimal, 2 places or null | omitted when empty | BaseCost is nil on a statement no adjustment reached. |
| `total` | decimal, 2 places | always |  |
| `size` | object | always |  |
| `stats` | object | always |  |
| `received_at` | string, RFC 3339 UTC | always |  |

#### `Period`

Period is the half-open interval the document bills, both ends in UTC.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `from` | string, RFC 3339 UTC | always |  |
| `to` | string, RFC 3339 UTC | always |  |

#### `LineItem`

LineItem is one resource as the project is billed for it.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `resource_id` | string | always |  |
| `quantities` | object of decimal, 4 places | always | Quantities holds one entry per rated dimension. |
| `total` | decimal, 2 places | always |  |
| `related` | array of [LineItem](#lineitem) | omitted when empty |  |

#### `Grouped`

Grouped is declared in a group, so its comment sits on the spec rather than on the declaration.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `name` | string | always |  |
