### `Item`

One item as this API stores it.

A member the producer leaves out is absent rather than null.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | [Uuid](#uuid) | yes |  |
| `kind` | `draft`, `final` | yes | What the item is. |
| `note` | string or null | no | Free text, null when the item carries none. |
| `payload` | any | no | Whatever the producer sent. It is described rather than constrained, so that one bad member refuses one item. |
| `tags` | array of string | no | The labels the item carries. |

### `ItemList`

One page of items.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [Item](#item) | yes | The items of this page. |
| `next_cursor` | string or null | yes | Where the next page starts. It is null on the last page. |

### `Uuid`

A string, format `uuid`, matching `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`.
