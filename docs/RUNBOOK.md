<!--
doc: RUNBOOK
last-refreshed: 2026-09-09
generated-by: doc-refresh skill
-->

# Runbook — gmc-exporter

> **You were just paged. Start here.**

`gmc-exporter` reads a Geiger counter over a USB serial cable. It publishes the
readings to things like Prometheus and InfluxDB. If it is down, you are losing
radiation measurements. Nothing else breaks.

**This service does not need to be fixed urgently at 2am.** It records data. It
does not serve users. If you are tired, collect the output of step 1 and go back
to bed.

## 1. Is the service alive?

Run these three commands. Copy the output somewhere before changing anything.

```bash
# 1. Is the process running?
docker ps --filter name=gmc-exporter

# 2. Is it producing readings? 200 = yes, 503 = no
curl -s -w '\nHTTP %{http_code}\n' http://localhost:9101/readyz

# 3. What does it say is wrong?
curl -s http://localhost:9101/health
```

A healthy `/readyz` returns exactly this:

```json
{"status":"ready"}
```

An unhealthy one names the reason:

```json
{"status":"not ready","reason":"device is not connected"}
```

Then read the last 50 log lines:

```bash
docker logs --tail 50 gmc-exporter
```

## 2. Service overview

| Property | Value |
|---|---|
| Port | `9101` |
| Liveness endpoint | `/healthz` — is the process up |
| Readiness endpoint | `/readyz` — is it producing data |
| Detailed health | `/health` — per-dependency status |
| Metrics | `/metrics` — Prometheus format |
| Log location | `docker logs gmc-exporter` (logs go to stdout) |
| Log format | JSON by default. Set `GOGMC_LOG_FORMAT=text` to read it by eye |
| Restart command | `docker restart gmc-exporter` |
| Deployed via | Docker image `dougeubanks/gmc-exporter` |
| Configuration | Environment variables, or a config file. See [ENV_VARS.md](./ENV_VARS.md) |
| Hardware | A GQ Electronics GMC-series Geiger counter on a USB serial port |

### What the three health endpoints mean

Do not confuse them. They answer different questions.

| Endpoint | Returns 503 when | Use it for |
|---|---|---|
| `/healthz` | Almost never. Only if the process is dead. | Container liveness probes |
| `/readyz` | Device disconnected, or no recent reading | Alerting, load balancer checks |
| `/health` | Same as `/readyz`, plus a reason | Humans and external monitors |

> `/healthz` deliberately ignores backend outages. A liveness check that fails
> when a database is down causes a restart loop, which is worse than the outage.

## 3. Start, stop, restart

```bash
# Start
docker start gmc-exporter

# Stop (graceful — the exporter exits with status 0)
docker stop gmc-exporter

# Restart
docker restart gmc-exporter
```

> **SECURITY:** If you are restarting because you suspect a security incident,
> do not restart in place. Stop the container, leave it stopped, and preserve
> the logs with `docker logs gmc-exporter > /tmp/incident.log`. Escalate before
> bringing it back online.

### Exit codes

| Code | Meaning | Action |
|---|---|---|
| `0` | Clean shutdown, or `--version` was used | None. This is normal |
| `1` | Startup failure. Bad configuration, or a sink that cannot be built | Read the last log line. It names the setting |

The exporter exits `0` on `SIGTERM`. That matters: watchdog scripts commonly
treat `0`, `143` and `137` as a deliberate stop, and anything else as a crash.
An exporter that exits non-zero on a normal stop gets restarted by watchdogs
during planned maintenance.

**No other exit codes exist.** A poll failure, an unreachable backend, a
corrupted reading, or a panic inside a sink will not stop the process.

## 4. Known failure modes

Ordered by how often they happen.

| Symptom | Root cause | Immediate fix |
|---|---|---|
| `readyz` 503, `"device is not connected"` | Adapter unplugged, or the container cannot open it | Step 5 |
| Log: `permission denied` on the serial device | Container is not in the device's group | Step 5.2. The error message names the group |
| Log: `radmon.org refused a submission as too soon` | Submitting faster than the service allows | Nothing. The sink pauses itself and recovers |
| Log: `write failed, retrying` | A backend was briefly unavailable | Nothing. It retries. Check the backend if it repeats |
| Log: `unexpected response length` or `link desynchronized` | Electrical noise on the serial cable | Nothing. That reading is skipped. See step 6 if frequent |
| Log: `decoded value is outside the plausible range` | A corrupted byte produced an impossible reading | Nothing. That reading is skipped |
| `readyz` 503, `"readings are stale"` | The device stopped answering | Step 5 |
| `readyz` 503, `"no successful reading yet"` | Just started, or has never succeeded | Wait one poll interval, then step 5 |
| Log: `poll cycle panicked, reading skipped` | A bug. Should never happen | Step 8. Please file it |
| Container will not start, exits `1` | Invalid configuration | Read the last log line. It names the setting |

### Reading the logs

Default output is JSON, one object per line. To make it readable:

```bash
# If you have jq
docker logs --tail 50 gmc-exporter | jq -r '"\(.time) \(.level) \(.msg)"'

# If you do not, show only problems
docker logs --tail 200 gmc-exporter | grep -v '"level":"INFO"'
```

**A healthy exporter produces no output above `INFO`.** If that last command
prints nothing, there is nothing wrong.

## 5. The device is not connected

This is the most common failure. Work through it in order.

### 5.1 Is the hardware there?

```bash
# On the host, not in the container
ls -l /dev/ttyUSB0
```

| Result | Meaning | Fix |
|---|---|---|
| `crw-rw---- 1 root dialout ...` | Present. Go to 5.2 | — |
| `No such file or directory` | The adapter is not detected | Reseat the USB cable. Then `dmesg | tail -20` |

If the node appears under a different name, such as `/dev/ttyUSB1`, set
`GOGMC_SERIAL_PORT` to that path and restart.

### 5.2 Can the container open it?

The exporter runs as a non-root user. It must be in the group that owns the
device node.

1. Find the group number:

   ```bash
   stat -c '%g' /dev/ttyUSB0
   ```

   This is usually `16`, the `dialout` group.

2. Confirm the container was given that group:

   ```bash
   docker inspect gmc-exporter --format 'groupAdd={{.HostConfig.GroupAdd}}'
   ```

3. If the list does not contain the number from step 1, recreate the container
   with `--group-add <number>`.

The error message already tells you this. It looks like:

```
open /dev/ttyUSB0: permission denied (this process runs as uid 65532, gid 65532;
/dev/ttyUSB0 is owned by group 16. Pass the device through and join that group:
  docker run --device=/dev/ttyUSB0 --group-add=16 ...)
```

### 5.3 Is something else holding the port?

The serial link is exclusive. Two processes reading it produce interleaved
nonsense that looks exactly like a protocol bug.

```bash
fuser -v /dev/ttyUSB0
```

Exactly one process should be listed. If there are two, stop the one you do not
want. A common cause is an older exporter being restarted by a watchdog.

### 5.4 Recovery is automatic

You usually do not need to restart anything. When the device disappears, the
exporter logs the error, drops the connection, and retries with backoff. Plug
the adapter back in and it reconnects on the next poll.

## 6. Frequent corrupted readings

Occasional skipped readings are normal. The serial link runs with no parity and
the protocol has no checksum, so electrical noise is undetectable except by
sanity-checking the decoded value.

Measured on healthy hardware: roughly 1 bad exchange in 400 under aggressive
polling, and none at a 60-second interval over several hundred polls.

If you are seeing more than about 1 in 100:

1. Reseat the USB cable at both ends.
2. Move the cable away from mains wiring and switching power supplies.
3. Try a shorter cable, or one with a ferrite bead.
4. Try a different USB port, ideally not behind a hub.

Count them:

```bash
docker logs gmc-exporter 2>&1 | grep -c "reading skipped"
```

## 7. Rollback

Images are tagged with an exact version, a minor version, a major version, and
`latest`. Rolling back is running an older tag. The exporter stores no state and
never writes to the device, so there is nothing to migrate.

```bash
# 1. See what is running now
docker inspect gmc-exporter --format '{{.Config.Image}}'

# 2. Pull the version you want
docker pull dougeubanks/gmc-exporter:0.2.1

# 3. Replace the container. Keep every other flag identical
docker stop gmc-exporter && docker rm gmc-exporter
docker run -d --name gmc-exporter --restart=unless-stopped \
  --device=/dev/ttyUSB0 --group-add="$(stat -c '%g' /dev/ttyUSB0)" \
  -p 9101:9101 \
  dougeubanks/gmc-exporter:0.2.1

# 4. Confirm
docker logs --tail 20 gmc-exporter
curl -s http://localhost:9101/readyz
```

> **SECURITY:** Do not add `--privileged` to make a permissions problem go away.
> It grants every capability on the host. Use `--device` with `--group-add`
> instead. See [Device access](../README.md#device-access).

Readings resume on the next poll. Data written before the rollback is unaffected.

## 8. Escalation path

1. Collect the evidence:

   ```bash
   docker logs --tail 200 gmc-exporter > /tmp/gmc-exporter.log
   curl -s http://localhost:9101/health >> /tmp/gmc-exporter.log
   docker inspect gmc-exporter > /tmp/gmc-exporter-inspect.json
   ```

2. Check whether the reading itself is plausible. Compare `/metrics` against the
   number on the counter's own screen. Background is normally tens of CPM.

3. If unresolved, open an issue at
   <https://github.com/RealDougEubanks/gmc-exporter/issues> with the log excerpt.

> **SECURITY:** Before attaching logs to an issue, confirm they contain no
> credentials. The exporter redacts them, but check anyway. Search your log file
> for your passwords and tokens before sharing it.

## 9. Related documents

| Document | Use it for |
|---|---|
| [README.md](../README.md) | What this is, how to deploy it, sink list |
| [ENV_VARS.md](./ENV_VARS.md) | Every configuration setting |
| [protocol-measurements.md](./protocol-measurements.md) | Why the hardware behaves as it does |
| [../SECURITY.md](../SECURITY.md) | Credential handling, reporting a vulnerability |
