# gmc-exporter

Reads a **GQ Electronics GMC-series Geiger counter** over USB serial and
publishes its readings to whichever telemetry backends you enable.

Built to run unattended for months. A partial serial read, an unplugged adapter,
an unreachable backend or a panic inside a client library all degrade to a
logged, skipped reading rather than a dead container.

- **Source and full documentation:** https://github.com/RealDougEubanks/gmc-exporter
- **Licence:** MIT
- **Image size:** ~7 MB compressed, ~18 MB on disk. Distroless, non-root, static binary.
- **Platforms:** `linux/amd64`, `linux/arm64`

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

## Device access — read this first

This is the one thing that catches people out. The container runs as a non-root
user, and reading `/dev/ttyUSB0` needs **two** separate permissions: the kernel's
device cgroup must allow the container the device, and the process must be able
to open a node owned by `root:dialout`.

Measured against a real device node:

| Device passed as | `--privileged` | `--group-add` | Works |
|---|---|---|---|
| `--device` | no | yes | **yes — recommended** |
| volume mount (`-v`) | yes | yes | yes |
| volume mount | yes | no | no |
| volume mount | no | yes | no |

A volume mount makes the node visible but grants no device permission, which is
why that route still needs `--privileged`. Use `--device` and you can leave
privileged mode off entirely.

If it cannot open the device, the container says exactly what to add — naming the
group the node actually belongs to — and keeps retrying rather than exiting.

## Sinks

Every sink is **disabled by default** and independently toggleable. Each is
individually timed out, published to concurrently, and unable to stop the poll
loop or affect another sink.

| Sink | Enable with | Credentials |
|---|---|---|
| Prometheus | `GOGMC_PROMETHEUS_ENABLED` | none |
| InfluxDB 1.x | `GOGMC_INFLUX1_ENABLED` | optional user/password |
| InfluxDB 2.x | `GOGMC_INFLUX2_ENABLED` | token, org, bucket |
| OpenTelemetry (OTLP) | `GOGMC_OTLP_ENABLED` | optional headers |
| MQTT + Home Assistant discovery | `GOGMC_MQTT_ENABLED` | optional user/password |
| radmon.org | `GOGMC_RADMON_ENABLED` | user + data-sending password |
| GMCMAP.com | `GOGMC_GMCMAP_ENABLED` | account ID + counter ID |
| Safecast | `GOGMC_SAFECAST_ENABLED` | API key |

**Start with Prometheus.** It needs no credentials at all, cannot fail in a way
that stalls the poll loop, and keeps working if you change every other backend.

## Endpoints

| Path | Purpose |
|---|---|
| `/metrics` | Prometheus scrape |
| `/healthz` | Liveness. Never fails because a backend is down |
| `/readyz` | Readiness. 503 if the device is disconnected or readings are stale |
| `/health` | Per-dependency detail. Carries no URLs, hostnames or tokens |

## Configuration

Everything is set by environment variable, prefixed `GOGMC_`. Every secret also
accepts a `_FILE` variant, so it can be delivered as a Docker or Kubernetes
secret rather than as a plain variable visible to `docker inspect`:

```bash
GOGMC_RADMON_PASSWORD_FILE=/run/secrets/radmon_password
```

A config file is optional and picked up automatically from
`/etc/gmc-exporter/config.ini` or `/config.ini`. Precedence is flags, then
environment, then file, then defaults.

Full reference:
[docs/ENV_VARS.md](https://github.com/RealDougEubanks/gmc-exporter/blob/main/docs/ENV_VARS.md)

## Tags

| Tag | Meaning |
|---|---|
| `latest` | Newest release |
| `1` | Newest 1.x |
| `1.0` | Newest 1.0.x |
| `1.0.0` | Exactly that version |

Pin to `1.0.0` for reproducibility, or `1` to get fixes without breaking changes.
Rolling back is just running an older tag: the exporter holds no state and never
writes to the device.

## Supported hardware

The protocol is GQ Electronics' own, documented in **GQ-RFC1201**, covering the
GMC-280, GMC-300, GMC-320 and later.

Developed and verified against a **GMC-320Re 4.62** on a CH340 USB-serial
bridge. Every response length was confirmed against real hardware before being
relied on, and the calibration table — which the vendor does not document — was
derived by measurement. The evidence is in
[docs/protocol-measurements.md](https://github.com/RealDougEubanks/gmc-exporter/blob/main/docs/protocol-measurements.md).

Thanks to GQ Electronics for publishing the protocol specification.

## More documentation

| Document | Use it for |
|---|---|
| [README](https://github.com/RealDougEubanks/gmc-exporter#readme) | Overview and deployment |
| [RUNBOOK](https://github.com/RealDougEubanks/gmc-exporter/blob/main/docs/RUNBOOK.md) | It broke and you are on call |
| [ENV_VARS](https://github.com/RealDougEubanks/gmc-exporter/blob/main/docs/ENV_VARS.md) | Every setting |
| [SECURITY](https://github.com/RealDougEubanks/gmc-exporter/blob/main/SECURITY.md) | Credential handling, reporting a vulnerability |
| [Issues](https://github.com/RealDougEubanks/gmc-exporter/issues) | Bugs and questions |
