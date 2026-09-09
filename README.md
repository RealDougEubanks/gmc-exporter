# gmc-exporter

Reads a GQ Electronics GMC-series Geiger counter over USB serial and publishes
its readings to whichever telemetry backends you enable.

Built to run unattended for months in a container. Every failure it can
encounter — a partial serial read, an unplugged adapter, an unreachable
backend, a panic inside a client library — degrades to a logged, skipped
reading rather than a dead process.

## Supported hardware

The protocol is GQ Electronics' own, documented in
[GQ-RFC1201](docs/GQ-RFC1201.txt), which covers the GMC-280, GMC-300, GMC-320
and later models.

Development and verification were done against a **GMC-320Re 4.62** on a CH340
USB-serial bridge. Every response length in the implementation was confirmed
against that device before being relied upon; the evidence is in
[docs/protocol-measurements.md](docs/protocol-measurements.md).

The protocol documentation is published by GQ Electronics LLC, and this project
would not exist without it.

## Why partial reads matter here

This is the single most important thing to understand about talking to these
devices, and it is measured rather than theoretical.

The CH340/CH341 bridges these counters ship with deliver at most 32 bytes per
USB packet. A `<GETCFG>>` response is 256 bytes, and it arrived as **exactly
eight separate reads on 120 of 120 attempts**. An implementation that issues one
`read()` and trusts the result gets 32 bytes where it expected 256, every single
time.

Smaller responses usually arrive whole, but not always: across 420 iterations a
14-byte `<GETVER>>` split into two reads once. At roughly 1 in 400 that is rare
enough to survive testing and common enough to happen daily in production.

Two further anomalies were captured under aggressive polling, each about 1 in
400: the device answered `<GETTEMP>>` with a full 256-byte configuration block,
and separately returned nothing at all. Both are handled as skipped readings.
The first is the dangerous one — reading only the 4 bytes expected would have
produced a plausible but entirely fabricated temperature — so a response longer
than the command expects is rejected outright.

A third fault showed up during a live run: a device reading 30.8 °C reported
94.8 °C for one poll. `1e 08 00 aa` against `5e 08 00 aa` — a single flipped
bit. The link is 8N1 with no parity and the protocol has no checksum, so that
reply had the right length and the right terminator and passed every structural
check. Decoded values therefore get plausibility bounds as well, which matters
most for voltage: its reply is one byte with no terminator, so one bad bit turns
4.2 V into 10.6 V undetectably.

## Quick start

```bash
docker run -d --name gmc-exporter \
  --device=/dev/ttyUSB0 \
  --group-add="$(stat -c '%g' /dev/ttyUSB0)" \
  -p 9101:9101 \
  -e GOGMC_PROMETHEUS_ENABLED=true \
  dougeubanks/gmc-exporter:latest
```

Then:

```bash
curl localhost:9101/metrics
curl localhost:9101/readyz
```

### Docker Compose

```yaml
services:
  gmc-exporter:
    image: dougeubanks/gmc-exporter:latest
    restart: unless-stopped
    devices:
      - /dev/ttyUSB0:/dev/ttyUSB0
    group_add:
      - dialout
    ports:
      - "9101:9101"
    environment:
      GOGMC_PROMETHEUS_ENABLED: "true"
      GOGMC_POLL_INTERVAL: 60s
      GOGMC_INFLUX1_ENABLED: "true"
      GOGMC_INFLUX1_URL: http://influxdb:8086
      GOGMC_INFLUX1_DATABASE: radiation
    secrets:
      - radmon_password

secrets:
  radmon_password:
    file: ./secrets/radmon_password
```

### `--device` instead of `--privileged`

Reading `/dev/ttyUSB0` does not require root. It requires membership of the
group that owns the device node, usually `dialout`. Passing `--device` plus
`--group-add` grants exactly that, and nothing else.

`--privileged` is often used here instead, but it grants every capability on the
host in order to solve a file-permission problem. If you are migrating a
privileged container, this is a free security improvement.

Note that bind-mounting the device (`-v /dev/ttyUSB0:/dev/ttyUSB0`) is *not*
equivalent: it makes the node visible but grants no cgroup device permission, so
it only works under `--privileged`. Use `--device`.

## Configuration

Everything is configured by environment variable, prefixed `GOGMC_`. See
[.env.example](.env.example) for the complete annotated list.

Every secret also accepts a `_FILE` variant, for example
`GOGMC_RADMON_PASSWORD_FILE=/run/secrets/radmon`. **Prefer it.** A plain
environment variable is visible to anyone who can run `docker inspect` and is
inherited by every child process. Setting both forms of the same setting is an
error rather than a silent preference.

Configuration is validated once at startup and reports every problem at once,
naming each offending setting, so a misconfigured deployment can be fixed in one
pass instead of one restart per mistake.

### Core settings

| Setting | Default | Notes |
|---|---|---|
| `GOGMC_SERIAL_PORT` | `/dev/ttyUSB0` | Device node |
| `GOGMC_SERIAL_BAUD` | `115200` | GMC-320 factory default |
| `GOGMC_POLL_INTERVAL` | `60s` | |
| `GOGMC_AVERAGE_WINDOW` | `60` | Samples in the running mean |
| `GOGMC_HTTP_ADDR` | `:9101` | Metrics and health |
| `GOGMC_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `GOGMC_LOG_FORMAT` | `json` | `json` or `text` |
| `GOGMC_CALIBRATION` | *(from device)* | Override, e.g. `60:0.39,240:1.56,1000:6.5` |

### Location

`GOGMC_LATITUDE` and `GOGMC_LONGITUDE` are shared by every sink that needs
coordinates. They are optional and must be set together.

**This data is published publicly.** It is attached to every reading sent to a
public radiation map and cannot be recalled. Sinks that require coordinates
refuse to start without them rather than substituting a default, because a
default would silently publish your readings attributed to the wrong place.

## Sinks

Every sink is **disabled by default**, independently toggleable, individually
timed out, published to concurrently, and incapable of stopping the poll loop or
affecting another sink.

| Sink | Enable with | Needs location | Credentials |
|---|---|---|---|
| Prometheus | `GOGMC_PROMETHEUS_ENABLED` | No | None |
| InfluxDB 1.x | `GOGMC_INFLUX1_ENABLED` | No | Optional user/password |
| InfluxDB 2.x | `GOGMC_INFLUX2_ENABLED` | No | Token, org, bucket |
| OpenTelemetry (OTLP) | `GOGMC_OTLP_ENABLED` | No | Optional headers |
| MQTT + Home Assistant | `GOGMC_MQTT_ENABLED` | No | Optional user/password |
| radmon.org | `GOGMC_RADMON_ENABLED` | Only with `_USE_LATLNG` | User + data-sending password |
| GMCMAP.com | `GOGMC_GMCMAP_ENABLED` | Yes | Account ID + counter ID |
| Safecast | `GOGMC_SAFECAST_ENABLED` | Yes | API key |

**Prometheus is the one to reach for first.** It needs no credentials at all,
cannot fail in a way that stalls the poll loop, and keeps working if you change
every other backend. Grafana Alloy and Telegraf both scrape it.

### radmon.org

radmon uses a separate **data-sending password**, distinct from your account
login password. Set that one.

Its API is GET-only, so credentials necessarily appear in the request URL. Over
HTTPS the query string is encrypted in transit, so the risk is not interception
— it is log hygiene. Go embeds the full URL in every `*url.Error`, so naively
logging a transport failure writes the password to disk. Every transport error
in this project is redacted before it can be logged, and there are tests
asserting no credential reaches log output.

**On submission rate.** radmon.org rejects submissions that arrive too close
together, answering HTTP 200 with a body of `Too soon`. That is a success status
carrying a failure, so an implementation that only checks the status code will
report every one of those as a successful submission.

A rejected submission is never retried. Retrying cannot succeed — the server is
saying "not yet" — and each attempt restarts the interval, so retries turn one
rejection into a permanent loop where every subsequent poll is also too soon.
The reading is dropped and the next poll submits on schedule.

If you see occasional `Too soon` rejections at a 60 second poll interval, raise
`GOGMC_POLL_INTERVAL` slightly. Nothing is lost when one is skipped, and the
Prometheus and InfluxDB sinks still record every reading.

### Replacing an existing InfluxDB 1.x exporter

If you already have history in InfluxDB, landing in the same series matters more
than having tidy field names: dashboards and alerts built on the old data stop
matching silently, with nothing reporting an error.

```
GOGMC_INFLUX1_MEASUREMENT=data        # whatever your existing measurement is
GOGMC_INFLUX1_FIELD_STYLE=legacy      # CPM, ACPM, USV, Voltage, Temperature
GOGMC_INFLUX1_TAG_DEVICE=true         # serial and version tags
```

The default `snake` style writes `cpm`, `acpm`, `usvh`, `volts` and `temp_c`,
matching the Prometheus and MQTT sinks. Use it for a new database.

This option is InfluxDB 1.x only, because the schema it reproduces only ever
existed there.

### Home Assistant

With `GOGMC_MQTT_HA_DISCOVERY=true`, the sensors appear automatically, grouped
as a single device. A retained availability topic with a last-will-and-testament
means Home Assistant marks them unavailable when the exporter stops, rather than
showing a stale reading forever.

## Metrics and health

| Endpoint | Purpose |
|---|---|
| `/metrics` | Prometheus scrape |
| `/healthz` | Liveness. Is the process alive? |
| `/readyz` | Readiness. Is the device connected and are readings current? |
| `/health` | Detailed per-dependency status, `503` when unhealthy |

`/healthz` deliberately checks nothing but the process. Liveness that depends on
a backend turns a broker outage into a restart loop, which is worse than the
outage.

`/health` reports which sinks are configured but carries no URLs, hostnames or
tokens. It is unauthenticated so external monitors can reach it, which means it
must not double as a reconnaissance tool.

## Unraid

Add a container with:

| Field | Value |
|---|---|
| Repository | `dougeubanks/gmc-exporter:latest` |
| Device | `/dev/ttyUSB0` |
| Extra parameters | `--group-add 16` (use `stat -c '%g' /dev/ttyUSB0` to confirm the GID) |
| Port | `9101` |
| Variables | `GOGMC_*` as above |

Leave **Privileged off**. This was verified on a real Unraid host: the exporter
ran as `nonroot` with `--device` plus `--group-add`, reading the counter for
several minutes with no elevated privileges.

Note that Unraid's template UI often maps a device by adding a *volume* mapping
for `/dev/ttyUSB0`. That is not the same thing — a bind mount makes the node
visible but grants no cgroup device permission, which is why such setups need
`--privileged` to work at all. Use the Device field, not a path mapping.

Configuration is by environment variable, so there is no config file to mount.

### Rolling back

The image is versioned, and every release publishes exact, minor and major tags
alongside `latest`. To pin or roll back, change the repository tag:

```bash
# Pin to an exact version
docker pull dougeubanks/gmc-exporter:0.1.0

# Roll back one patch release
docker stop gmc-exporter && docker rm gmc-exporter
docker run -d --name gmc-exporter ... dougeubanks/gmc-exporter:0.1.0
```

Nothing in this exporter writes to the device or migrates any state, so rolling
back is just running the older tag. Readings resume on the next poll.

## Building

```bash
go build ./cmd/gmc-exporter
go test -race ./...
```

The test suite runs with **no hardware attached**. Protocol tests replay byte
sequences captured from a real device, including the split reads and both
anomalies, so CI exercises genuine wire behaviour on a runner with no Geiger
counter.

There is an opt-in hardware test, skipped by default:

```bash
GOGMC_SERIAL=/dev/ttyUSB0 go test ./internal/gmc/ -run TestHardware -v
```

### `gmc-probe`

`cmd/gmc-probe` is the measurement tool used to characterise the device. It
issues each read-only command repeatedly and records every `read()` return
separately, so response lengths and chunking are observed rather than assumed.
It sends read-only commands exclusively and cannot alter the device.

```bash
go build ./cmd/gmc-probe
sudo ./gmc-probe -device /dev/ttyUSB0 -iterations 20 -out capture.json
```

Note the serial port is exclusive: stop anything else using the device first, or
you will get interleaved output that looks exactly like a protocol bug.

## Licence

MIT. See [LICENSE](LICENSE).

The GQ-RFC1201 protocol specification is copyright GQ Electronics LLC and is
included here for reference; the implementation is original work written from
that specification and from direct measurement of the hardware.
