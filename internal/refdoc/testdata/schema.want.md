`Fixture catalog`, an object.

#### `root`

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `version` | string | yes | minLength 1 |
| `kind` | enum | no | `draft`, `final` |
| `tags` | array of string | no | minItems 1 |
| `entries` | object | yes | minProperties 1; values object (minProperties 1; values [entry](#entry)) |

No other property is allowed.

#### `entry`

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `measures` | array of [measure](#measure) | yes | minItems 1 |
| `window` | [window](#window) | no | none |
| `note` | string | no | maxLength 500 |

No other property is allowed.

#### `measure`

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `metric` | string | yes | minLength 1 |
| `kind` | enum | yes | `gauge`, `counter` |
| `hourly` | [rate](#rate) | no | none |
| `each` | [rate](#rate) | no | none |
| `either` | alternatives | no | none |

Other properties are [rate](#rate).

Exactly one of these alternatives holds:

- `hourly` is required, `each` is absent, `kind` is `gauge`
- `each` is required, `hourly` is absent, `kind` is `counter`

#### `labels`

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `name` | string | yes | none |

Other properties are `string`.

#### `window`

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `from` | string | yes | none |
| `to` | string | no | none |

Other properties are allowed.

#### `rate`

A number at least 0, or a string matching `^[0-9]+(\.[0-9]+)?$`.
