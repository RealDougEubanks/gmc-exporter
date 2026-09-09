<!--
doc: ASSUMPTIONS
last-refreshed: 2026-09-09
generated-by: doc-refresh skill
-->

# Assumptions and decisions

Non-obvious decisions, recorded per the Golden Rules in
[CLAUDE.md](../CLAUDE.md). Each entry states what was assumed, why, who recorded
it, and when.

Where a decision rests on measurement rather than documentation, the evidence is
in [protocol-measurements.md](./protocol-measurements.md).

---

## Protocol and hardware

### The calibration table layout in the configuration block

- **Assumption:** `<GETCFG>>` stores three calibration entries beginning at byte
  offset 8, each six bytes: a big-endian `uint16` count rate followed by a
  little-endian IEEE-754 `float32` dose rate.
- **Why:** GQ-RFC1201 section 7 documents only that the command returns 256
  bytes. It says nothing about the contents. The layout was derived by capturing
  100 byte-identical config blocks and testing every offset as a candidate pair.
  Exactly three offsets produced a sane relationship, and all three agreed on
  0.0065 µSv/h per CPM. It was corroborated independently against the previously
  deployed exporter's own output, which reported 28 CPM as 0.18 µSv/h.
- **Accepted risk:** This is an observation, not a vendor guarantee. A different
  firmware may store it elsewhere. `GOGMC_CALIBRATION` exists as the escape
  hatch, so a mismatch is a configuration change rather than a code change.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08

### Plausibility bounds on temperature and voltage

- **Assumption:** A decoded temperature outside −40 °C to 85 °C, or a voltage
  outside 0.5 V to 15 V, is corruption rather than a reading.
- **Why:** The link is 8N1 with no parity and the protocol has no checksum, so a
  flipped bit produces a response of the correct length with the correct
  terminator byte. Every structural check passes. A live run reported 94.8 °C
  from a device sitting at 30.8 °C: `1e 08 00 aa` against `5e 08 00 aa`, one bit
  apart. Across 418 captured replies the integer byte was `0x1f` every time.
- **Accepted risk:** A genuine extreme outside these ranges would be discarded.
  The ranges are deliberately wider than any plausible operating environment for
  a handheld instrument to make that unlikely.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08

### A response longer than expected is an error, not something to truncate

- **Assumption:** If the device returns more bytes than a command's reply should
  contain, the exchange is failed rather than the reply being cut to length.
- **Why:** Under aggressive polling, one `<GETTEMP>>` in 400 returned a full
  256-byte configuration block. Reading only the first four bytes would have
  produced a plausible, entirely fabricated temperature. Publishing invented
  data is worse than publishing nothing.
- **Cost:** One short read timeout per exchange, to confirm the device has
  stopped talking.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08

### Reconnect on transport errors only, never on malformed data

- **Assumption:** A short, absent, desynchronised, or implausible reply means the
  data is bad but the link is healthy. Only an error from the device node itself
  triggers a reconnect.
- **Why:** Malformed replies occur at roughly 1 in 400 under load. Reconnecting
  on those would discard a working port every few hundred reads, turning a
  skipped reading into a connection cycle.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08

### Direct termios rather than a serial library

- **Assumption:** Opening the device with `golang.org/x/sys/unix` and gating
  reads with `poll(2)` is preferable to a third-party serial package.
- **Why:** Two reasons. Precise read deadlines, without the decisecond
  granularity that `VMIN`/`VTIME` alone imposes. And avoiding the convention,
  present in some popular Go serial libraries, of reporting a read timeout as
  `(0, nil)`, which silently turns `io.ReadFull` into a busy loop. This
  implementation returns an explicit `ErrTimeout` so a caller cannot make that
  mistake.
- **Trade-off:** The device layer is Linux-only. A stub returns a clear error
  elsewhere, so the module still builds and tests on any platform.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08

---

## Security

### `redact.Secret` stores its value in a closure, not a string

- **Assumption:** A credential type must be unreadable by reflection, not merely
  by `fmt.Stringer`.
- **Why:** `fmt` walks unexported struct fields by reflection and cannot call
  methods on what it finds. With `type Secret string`, logging a whole sink
  struct prints the password in the clear:
  `fmt.Sprintf("%v", sink)` gives `{radmon.org someuser hunter2}`. Every sink
  keeps its credential in an unexported field, so that was the case that
  mattered. A function field has no readable contents.
- **Trade-off:** `Secret` is no longer comparable and cannot be created by
  conversion. It must be built with `redact.New`.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08

### The image runs as a non-root user by default

- **Assumption:** Defaulting to uid 65532 is worth requiring `--group-add` at
  deployment.
- **Why:** Reading a serial device needs membership of the device node's group,
  not root. The alternative default, running as root, would make migration from
  a privileged container seamless but would ship a worse security posture to
  everyone.
- **Accepted cost:** An existing privileged deployment must add `--group-add`.
  A permission failure names the exact group and flags, and remains non-fatal.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

### `/healthz` ignores backend state

- **Assumption:** Liveness reports only that the process is running. It never
  fails because a sink is unreachable.
- **Why:** A liveness probe that fails during a backend outage causes an
  orchestrator to restart the container repeatedly, which is worse than the
  outage. `/readyz` and `/health` answer the question about data freshness.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08

### `govulncheck` is not yet in CI

- **Assumption:** Running it manually before a release is acceptable for now.
- **Why:** Not a deliberate design choice, just work not yet done. Recorded so
  it is visible rather than forgotten.
- **Action:** Add a `govulncheck ./...` step to `.github/workflows/ci.yml`.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

---

## Sinks

### A radmon.org rate limit is permanent for the current reading

- **Assumption:** When radmon.org answers `Too soon`, that reading is dropped and
  the sink pauses itself for a doubling cooldown rather than retrying.
- **Why:** Retrying cannot succeed, because the server is saying "not yet". Worse,
  each attempt restarts the interval, so retries turn one rejection into a
  permanent loop where every subsequent poll is also too soon. That was observed
  in production before the fix. The service is free and volunteer-run on a
  Raspberry Pi; hammering it is not resilience.
- **Note:** The documented minimum interval is 30 seconds. The service documents
  no escalating penalty, so the cooldown discovers a workable cadence at runtime.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

### A radmon.org success is matched after stripping HTML

- **Assumption:** The success body is `OK` once `<br>` tags are removed.
- **Why:** The API thread documents the response as `OK`. The wire carries
  `OK<br>`. Comparing against the documented string alone rejected every
  successful submission, which failed in the worst available direction: the sink
  reported failure while the data was arriving. The tag is stripped rather than
  the check loosened to a prefix match, because `OK` prefixes plenty of
  sentences that are not successes.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

### Legacy InfluxDB field names are scoped to 1.x only

- **Assumption:** `GOGMC_INFLUX1_FIELD_STYLE=legacy` exists, and there is no
  equivalent for InfluxDB 2.x.
- **Why:** The schema being reproduced belongs to an older exporter that only
  ever wrote to InfluxDB 1.x. Offering it for 2.x would imply a compatibility
  need that does not exist.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

### A legacy config file implies the legacy InfluxDB schema

- **Assumption:** When the older exporter's `config.ini` schema is recognised,
  InfluxDB defaults to measurement `data` with legacy field names.
- **Why:** This is the point of recognising the schema at all. Without it, an
  upgrade would quietly start writing a new measurement with new field names,
  and every dashboard built on the existing history would go blank with nothing
  reporting an error. A silent data discontinuity is worse than a loud failure.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

### A config file is auto-detected at `/config.ini`

- **Assumption:** Searching `/etc/gmc-exporter/config.ini` then `/config.ini` is
  acceptable magic.
- **Why:** `/config.ini` is where the older exporter's file was mounted, so an
  existing deployment keeps working after swapping the image. The path found is
  logged at startup, so the behaviour is visible rather than mysterious.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

### No AWS CloudWatch sink

- **Assumption:** CloudWatch is not worth implementing without a specific request.
- **Why:** Custom metrics cost roughly $0.30 per metric per month, so five
  metrics is about $1.50 monthly in perpetuity for data Grafana already
  visualises locally at no cost. The AWS SDK is also a heavy dependency for an
  image intended to be tens of megabytes.
- **If revisited:** Put it behind a build tag so others do not pay the binary
  size cost.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08

### Safecast defaults to a 10 minute minimum publish interval

- **Assumption:** `GOGMC_SAFECAST_MIN_INTERVAL` defaults to `10m`, while every
  other sink defaults to publishing on each poll.
- **Why:** The useful poll interval and the appropriate publish interval are not
  the same number. Reading every 60 seconds gives local graphs resolution worth
  having. Sending all of it to Safecast is different: it is a permanent public
  archive built around mobile survey data, and a fixed sensor at 60 seconds
  contributes over 500,000 near-identical rows a year with no delete API. Ten
  minutes still yields 144 points a day.
- **Accepted risk:** A non-zero default for exactly one sink is inconsistent, and
  someone will be surprised that Safecast has fewer points than InfluxDB. That is
  documented in ENV_VARS.md, and `0` restores per-poll behaviour.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

### Throttling lives in the fan-out, not in each sink

- **Assumption:** `sink.Throttle` wraps a sink rather than each sink
  implementing its own rate limit.
- **Why:** It is identical logic every time, and a sink should not have to know
  how often the poll loop runs. Only a *successful* publish starts the interval;
  throttling a failure would cost the reading and then delay its retry by the
  whole interval, turning one lost point into many.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

### Gaps in a public map's history cannot be backfilled

- **Assumption:** Readings missed during downtime are lost from radmon.org,
  GMCMAP and Safecast.
- **Why:** radmon.org deliberately offers no bulk upload: "Bulk upload would come
  under data manipulation". Its purpose is live readings. No attempt is made to
  queue and replay missed submissions.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-09

---

## Tooling

### `errcheck`'s `check-blank` is disabled

- **Assumption:** Writing `_ = f()` is the correct way to ignore an error, and
  the linter should not flag it.
- **Why:** `.golangci.yml` recommends that idiom as the way to make ignoring an
  error a visible decision. Enabling `check-blank` flags exactly that pattern,
  which would push contributors towards dropping the call entirely. Genuinely
  unchecked calls are still reported.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08

### `noctx` is excluded from test files

- **Assumption:** Tests may call `http.Get` and `httptest.NewRequest` without a
  context.
- **Why:** `noctx` guards against production code hanging on a request with no
  deadline. A test against its own `httptest` server cannot hang in a way that
  matters, and threading a context through every helper would obscure what each
  test asserts.
- **Recorded by:** Claude Code (Sonnet 4.6)
- **Date:** 2026-09-08
