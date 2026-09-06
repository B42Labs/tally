| Variable | Type | Default | File-backed | Governs |
| --- | --- | --- | --- | --- |
| `TALLY_TEST_LOG_LEVEL` | string | `INFO` | no | LogLevel is the threshold, one of DEBUG \| INFO. |
| `TALLY_TEST_DB_URL` | string | none | yes (`TALLY_TEST_DB_URL_FILE`) | DBURL is the connection string. It has no default because a guessed database is worse than none. Supports the *_FILE convention. |
| `TALLY_TEST_HTTP_PORT` | integer | `8080` | no | HTTPPort is the port the server listens on. |
| `TALLY_TEST_DB_MAX_CONNS` | integer | `10` | no | DBMaxConns bounds the connection pool. |
| `TALLY_TEST_BUFFER_MAX_EVENTS` | integer | `1000000` | no | BufferMaxEvents bounds what the buffer holds before it refuses writes. |
| `TALLY_TEST_METRICS_ENABLED` | boolean | `true` | no | MetricsEnabled exposes the instrumentation. |
| `TALLY_TEST_CLOUDS` | list, comma-separated | `one,two` | no | Clouds are the clouds the process reconciles. |
