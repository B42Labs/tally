`Fixture adjustments`, an array.

#### `root`

Each item is an object. The array holds at least 1 item.

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `kind` | enum | yes | `discount`, `surcharge` |
| `rate` | string | yes | `^0(\.\d{1,6})?$` |
| `note` | string | no | maxLength 200 |

No other property is allowed.
