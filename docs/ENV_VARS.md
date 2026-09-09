<!--
doc: ENV_VARS
last-refreshed: 2026-09-09
generated-by: doc-refresh skill
-->

# Environment Variables

> **SECURITY:** Never log, share, or commit the values of settings marked
> **secret** in the tables below. If one is exposed, rotate it immediately at the
> service that issued it. Changing it here is not enough.

Every setting is named `GOGMC_` followed by the name in the tables below.

There are **76 settings**. You will not need most of them. To get running you
need one sink enabled, which for Prometheus is a single variable:

```bash
GOGMC_PROMETHEUS_ENABLED=true
```

Everything else has a working default.

## 1. How settings are resolved

Settings are read in this order. The first place a setting is found wins.

| Order | Source | Example |
|---|---|---|
| 1 | Command-line flag | `--config /etc/gmc-exporter/config.ini` |
| 2 | Environment variable | `GOGMC_POLL_INTERVAL=90s` |
| 3 | `_FILE` variant of the variable | `GOGMC_RADMON_PASSWORD_FILE=/run/secrets/pw` |
| 4 | Config file | `[radmon]` / `user = someone` |
| 5 | Built-in default | `60s` |

The environment beats the config file on purpose. One mounted file can then be
reused across deployments and overridden per container without editing it.

### Validation happens once, at startup

All problems are reported together, and each message names the offending
setting. You do not have to restart six times to find six mistakes.

If a setting is invalid the process exits with status `1` and prints, for
example:

```
gmc-exporter: invalid configuration:
  - GOGMC_POLL_INTERVAL: "sixty" is not a duration (try 60s, 2m, 1h)
  - radmon.org is enabled but GOGMC_RADMON_USER is not set
```

## 2. Supplying secrets safely

> **SECURITY:** A plain environment variable is visible to anyone who can run
> `docker inspect`, and is inherited by every child process. Prefer the `_FILE`
> form in production.

Every setting marked **secret** also accepts a `_FILE` suffix. The value is then
read from that path.

```bash
# Instead of this
GOGMC_RADMON_PASSWORD=hunter2

# Do this
GOGMC_RADMON_PASSWORD_FILE=/run/secrets/radmon_password
```

Trailing newlines are stripped, because secret files almost always end in one.

Setting both `GOGMC_X` and `GOGMC_X_FILE` is a startup error, not a silent
preference. Guessing which one you meant is how the wrong credential reaches
production.

### Where to get each secret

| Secret | Obtain from |
|---|---|
| `GOGMC_RADMON_PASSWORD` | Your radmon.org account settings. This is the separate **data-sending password**, not your login password |
| `GOGMC_INFLUX1_PASSWORD` | Whoever administers your InfluxDB 1.x instance |
| `GOGMC_INFLUX2_TOKEN` | InfluxDB 2.x UI, under Load Data then API Tokens. A write token for one bucket is enough |
| `GOGMC_MQTT_PASSWORD` | Your MQTT broker's user database |
| `GOGMC_GMCMAP_COUNTER_ID` | Your GMCMAP.com account, under your registered counter |
| `GOGMC_SAFECAST_API_KEY` | Your Safecast account at <https://api.safecast.org> |

## 3. Device settings

| Variable | Required | Default | Description |
|---|---|---|---|
| `SERIAL_PORT` | no | `/dev/ttyUSB0` | Device node the counter is on |
| `SERIAL_BAUD` | no | `115200` | Line rate. Factory default for a GMC-320 |
| `SERIAL_RESPONSE_TIMEOUT` | no | `2s` | Budget for one complete response. Measured worst case on real hardware was 171 ms |
| `SERIAL_READ_TIMEOUT` | no | `50ms` | How often the read loop wakes. Not a response budget |
| `SERIAL_RECONNECT_BACKOFF` | no | `2s` | First delay before reopening a lost device |
| `SERIAL_RECONNECT_BACKOFF_MAX` | no | `60s` | Longest delay between reconnect attempts |

Consumed in `internal/config/config.go` and used by `internal/serialport`.

## 4. Sampling settings

| Variable | Required | Default | Description |
|---|---|---|---|
| `POLL_INTERVAL` | no | `60s` | Time between readings. Must be longer than `SERIAL_RESPONSE_TIMEOUT` |
| `AVERAGE_WINDOW` | no | `60` | How many samples form the running average |
| `CALIBRATION` | no | *(read from device)* | Override the device's own table, as `cpm:usv` pairs |

`CALIBRATION` exists because the layout of the device's configuration block is
not documented by the vendor. It was determined by measurement. If a firmware
stores it elsewhere, set this instead of waiting for a code change:

```bash
GOGMC_CALIBRATION=60:0.39,240:1.56,1000:6.5
```

## 5. Logging and HTTP settings

| Variable | Required | Default | Description |
|---|---|---|---|
| `LOG_LEVEL` | no | `info` | One of `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | no | `json` | `json` for machines, `text` for humans |
| `HTTP_ADDR` | no | `:9101` | Address for `/metrics` and the health endpoints |
| `HTTP_READ_TIMEOUT` | no | `10s` | Request read timeout |
| `HTTP_SHUTDOWN_TIMEOUT` | no | `5s` | Grace period for in-flight requests on shutdown |
| `HTTP_STALE_AFTER` | no | *(3 poll intervals)* | Age of the last good reading before `/readyz` fails |

## 6. Location settings

| Variable | Required | Default | Description |
|---|---|---|---|
| `LATITUDE` | for some sinks | *(unset)* | Decimal degrees, `-90` to `90` |
| `LONGITUDE` | for some sinks | *(unset)* | Decimal degrees, `-180` to `180` |

> **SECURITY:** These coordinates are **published publicly**. They are attached
> to every reading sent to a public radiation map and cannot be recalled. Decide
> how precisely you want your home address to be discoverable before setting
> them. Rounding to two decimal places is roughly a kilometre.

They must be set together. Required by GMCMAP, Safecast, and radmon when
`RADMON_USE_LATLNG=true`. A sink that needs them refuses to start without them
rather than sending a default, because a default silently attributes your
readings to the wrong place.

## 7. Sink settings

**Every sink is disabled by default.** Enable only what you use.

### 7.1 Prometheus

Needs no credentials and cannot stall the poll loop. Start here.

| Variable | Required | Default | Description |
|---|---|---|---|
| `PROMETHEUS_ENABLED` | no | `false` | Serve `/metrics` |
| `PROMETHEUS_PATH` | no | `/metrics` | Path to serve on. Must start with `/` |

### 7.2 radmon.org

| Variable | Required | Default | Description |
|---|---|---|---|
| `RADMON_ENABLED` | no | `false` | Enable this sink |
| `RADMON_USER` | if enabled | — | Your radmon.org username |
| `RADMON_PASSWORD` | if enabled | — | **secret.** The data-sending password |
| `RADMON_USE_LATLNG` | no | `false` | Also publish coordinates. Requires `LATITUDE` and `LONGITUDE` |
| `RADMON_MIN_INTERVAL` | no | `0` | Minimum time between publishes. `0` = every poll |
| `RADMON_TIMEOUT` | no | `15s` | Per-request timeout |
| `RADMON_RETRIES` | no | `2` | Retries after the first attempt |

> **SECURITY:** The radmon.org API accepts credentials only in the URL query
> string. That means the password appears in the request URL. Over HTTPS it is
> encrypted in transit, so the risk is log hygiene rather than interception. This
> exporter redacts every transport error before it can be logged. Do not add
> logging that prints request URLs.

radmon enforces a minimum of 30 seconds between submissions. Faster attempts get
`Too soon`, after which this sink pauses itself and retries later. No action
needed.

### 7.3 InfluxDB 1.x

| Variable | Required | Default | Description |
|---|---|---|---|
| `INFLUX1_ENABLED` | no | `false` | Enable this sink |
| `INFLUX1_URL` | if enabled | — | Base URL, e.g. `http://influxdb:8086` |
| `INFLUX1_DATABASE` | if enabled | — | Database to write to |
| `INFLUX1_USER` | no | — | Omit both user and password for an unauthenticated instance |
| `INFLUX1_PASSWORD` | no | — | **secret** |
| `INFLUX1_MEASUREMENT` | no | `radiation` | Measurement name |
| `INFLUX1_FIELD_STYLE` | no | `snake` | `snake` or `legacy`. See below |
| `INFLUX1_TAG_DEVICE` | no | `true` | Tag points with the device serial and version |
| `INFLUX1_MIN_INTERVAL` | no | `0` | Minimum time between publishes. `0` = every poll |
| `INFLUX1_TIMEOUT` | no | `15s` | Per-request timeout |
| `INFLUX1_RETRIES` | no | `2` | Retries after the first attempt |

`FIELD_STYLE` controls the field names:

| Style | Fields written |
|---|---|
| `snake` | `cpm`, `acpm`, `usvh`, `volts`, `temp_c` |
| `legacy` | `CPM`, `ACPM`, `USV`, `Voltage`, `Temperature` |

Use `legacy` when replacing an older exporter, so existing dashboards keep
matching. See [Migrating](../README.md#migrating-from-the-older-gogmc320-exporter).

### 7.4 InfluxDB 2.x

| Variable | Required | Default | Description |
|---|---|---|---|
| `INFLUX2_ENABLED` | no | `false` | Enable this sink |
| `INFLUX2_URL` | if enabled | — | Base URL, e.g. `http://influxdb2:8086` |
| `INFLUX2_TOKEN` | if enabled | — | **secret.** A write token |
| `INFLUX2_ORG` | if enabled | — | Organisation name or ID |
| `INFLUX2_BUCKET` | if enabled | — | Bucket to write to |
| `INFLUX2_MEASUREMENT` | no | `radiation` | Measurement name |
| `INFLUX2_MIN_INTERVAL` | no | `0` | Minimum time between publishes. `0` = every poll |
| `INFLUX2_TIMEOUT` | no | `15s` | Per-request timeout |
| `INFLUX2_RETRIES` | no | `2` | Retries after the first attempt |

### 7.5 OpenTelemetry (OTLP)

| Variable | Required | Default | Description |
|---|---|---|---|
| `OTLP_ENABLED` | no | `false` | Enable this sink |
| `OTLP_PROTOCOL` | no | `grpc` | `grpc` or `http` |
| `OTLP_ENDPOINT` | if enabled | — | Collector address, e.g. `alloy:4317` |
| `OTLP_HEADERS` | no | — | **treat as secret.** Comma-separated `key=value` pairs |
| `OTLP_INSECURE` | no | `false` | Disable TLS. Only for a trusted network |
| `OTLP_TIMEOUT` | no | `15s` | Export timeout |

> **SECURITY:** `OTLP_HEADERS` commonly carries an authorization token. Its
> values are never logged, only the header names. Use the `_FILE` form where
> your platform supports it.

### 7.6 MQTT and Home Assistant

| Variable | Required | Default | Description |
|---|---|---|---|
| `MQTT_ENABLED` | no | `false` | Enable this sink |
| `MQTT_BROKER` | if enabled | — | Broker hostname or IP |
| `MQTT_PORT` | no | `1883` | Broker port. Usually `8883` with TLS |
| `MQTT_TLS` | no | `false` | Connect with TLS |
| `MQTT_CA_CERT` | no | — | Path to a CA certificate. Requires `MQTT_TLS=true` |
| `MQTT_CLIENT_CERT` | no | — | Client certificate for mutual TLS |
| `MQTT_CLIENT_KEY` | no | — | Client key. Must be set with `MQTT_CLIENT_CERT` |
| `MQTT_USERNAME` | no | — | Broker username |
| `MQTT_PASSWORD` | no | — | **secret.** Broker password |
| `MQTT_CLIENT_ID` | no | `gmc-exporter` | MQTT client ID |
| `MQTT_QOS` | no | `1` | Quality of service, `0` to `2` |
| `MQTT_BASE_TOPIC` | no | `gmc-exporter` | Prefix for state topics |
| `MQTT_RETAIN` | no | `true` | Retain state messages |
| `MQTT_HA_DISCOVERY` | no | `false` | Publish Home Assistant discovery messages |
| `MQTT_HA_PREFIX` | no | `homeassistant` | Home Assistant discovery prefix |
| `MQTT_DEVICE_NAME` | no | `Geiger Counter` | Device name shown in Home Assistant |
| `MQTT_TIMEOUT` | no | `15s` | Per-publish timeout |

> **SECURITY:** `MQTT_TLS=false` sends the username and password in clear text
> over your network. Only acceptable on a network you fully control. Set
> `MQTT_TLS=true` if the broker supports it.

### 7.7 GMCMAP.com

| Variable | Required | Default | Description |
|---|---|---|---|
| `GMCMAP_ENABLED` | no | `false` | Enable this sink |
| `GMCMAP_ACCOUNT_ID` | if enabled | — | Your GMCMAP account ID |
| `GMCMAP_COUNTER_ID` | if enabled | — | **secret.** Your registered counter ID. This is the real credential |
| `GMCMAP_MIN_INTERVAL` | no | `0` | Minimum time between publishes. `0` = every poll |
| `GMCMAP_TIMEOUT` | no | `15s` | Per-request timeout |
| `GMCMAP_RETRIES` | no | `2` | Retries after the first attempt |

Requires `LATITUDE` and `LONGITUDE`. Publishes to a public map.

> **SECURITY:** The counter ID is the credential, not the account ID. Measured
> against the live service, a wrong account ID with a valid counter ID is
> accepted, while a wrong counter ID is rejected. Anyone holding the counter ID
> can publish readings attributed to your counter, so treat it like a password
> and supply it with `GOGMC_GMCMAP_COUNTER_ID_FILE`.

### 7.8 Safecast

| Variable | Required | Default | Description |
|---|---|---|---|
| `SAFECAST_ENABLED` | no | `false` | Enable this sink |
| `SAFECAST_API_KEY` | if enabled | — | **secret.** Your Safecast API key |
| `SAFECAST_DEVICE_ID` | no | `0` | Registered device ID. Omitted when `0` |
| `SAFECAST_MIN_INTERVAL` | no | **`10m`** | Minimum time between submissions. See note below |
| `SAFECAST_TIMEOUT` | no | `15s` | Per-request timeout |
| `SAFECAST_RETRIES` | no | `2` | Retries after the first attempt |

Requires `LATITUDE` and `LONGITUDE`. Contributes to a public open dataset.

> **Note:** `SAFECAST_MIN_INTERVAL` is the one sink setting with a non-zero
> default. Safecast is a permanent public archive designed around mobile survey
> data, where dense sampling maps a route. A fixed sensor publishing every 60
> seconds would add more than 500,000 near-identical rows per year, and there is
> no delete API. Ten minutes still gives 144 points a day. Set `0` to publish on
> every poll.

## 8. Verifying what is actually loaded

The startup log lists every resolved setting and where it came from, with
secrets replaced by `REDACTED`:

```bash
docker run --rm -e GOGMC_LOG_LEVEL=debug -e GOGMC_PROMETHEUS_ENABLED=true \
  dougeubanks/gmc-exporter:latest 2>&1 | head -40
```

Or check a running container:

```bash
curl -s http://localhost:9101/health
```

> **SECURITY:** `/health` deliberately reports which sinks are configured but no
> URLs, hostnames, or tokens. It is unauthenticated so external monitors can
> reach it, which means it must not become a reconnaissance tool. Do not add
> connection details to it.

## 9. Related documents

| Document | Use it for |
|---|---|
| [.env.example](../.env.example) | A copyable annotated file |
| [RUNBOOK.md](./RUNBOOK.md) | What to do when it breaks |
| [../SECURITY.md](../SECURITY.md) | Credential rules, reporting a vulnerability |
| [../README.md](../README.md) | Deployment and sink overview |
