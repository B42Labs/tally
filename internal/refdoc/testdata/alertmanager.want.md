| Setting | Value |
| --- | --- |
| Receiver | `default` |
| Group by | `alertname`, `cloud` |
| Group wait | `30s` |
| Group interval | `5m` |
| Repeat interval | `4h` |

| Matchers | Overrides |
| --- | --- |
| `severity="critical"` | `repeat_interval: 1h` |

The receivers are `default`; none carries an integration.
