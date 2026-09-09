<!--
doc: PROTOCOL-MEASUREMENTS
last-refreshed: 2026-09-09
generated-by: doc-refresh skill
-->

# Protocol measurements

Everything this exporter believes about the GMC serial protocol comes from one
of two places: the vendor specification [GQ-RFC1201](./GQ-RFC1201.txt), or
direct measurement of a real device. This document records the measurements, so
that every claim in the code can be traced to evidence rather than assumption.

Where the specification and the device disagree, the device wins.

## Device under test

| Property | Value | Source |
|---|---|---|
| Model | GMC-320 | `<GETVER>>` |
| Host | Unraid server, `x86_64` | `uname -a` |
| Device node | `/dev/ttyUSB0` | `ls -l` |
| USB bridge | QinHeng CH340, USB ID `1a86:7523` | `lsusb` |
| Kernel driver | `ch341-uart` | `dmesg` |
| Line settings | 115200 baud, 8N1, no flow control | GQ-RFC1201, "Serial Port configuration" |

Note the bridge is a **CH340**, not the CH341 it is often described as. They
share the `ch341-uart` driver and the same 32-byte bulk endpoint, so the
behaviour below applies to both, but the part number is recorded here accurately.

## How the captures were taken

`cmd/gmc-probe` issues each read-only command repeatedly and records every
`read(2)` return separately: how many bytes it carried, its contents, and how
long after the command was written it arrived. It never stops at the length the
specification predicts, so a response *longer* than documented is visible rather
than silently truncated.

The probe sends read-only commands exclusively. `POWEROFF`, `REBOOT`,
`FACTORYRESET`, `ECFG`, `WCFG` and the `SETDATE`/`SETTIME` family are not
implemented anywhere in this project.

The serial port is exclusive, so the previously deployed container was stopped
for the duration of each capture and restarted immediately afterwards.

| Capture | Contents | Fixture |
|---|---|---|
| Baseline | 20 iterations of all 8 commands, 120 ms apart | `internal/gmc/testdata/captures/baseline-20iter.json` |
| Aggressive | 400 iterations of 4 commands, back to back, no pause | `internal/gmc/testdata/captures/aggressive-400iter.json` |
| Config | 100 iterations of `<GETCFG>>`, back to back | `internal/gmc/testdata/captures/getcfg-100iter.json` |

These files are the raw probe output, committed verbatim. The protocol tests
replay all 1,860 recorded exchanges, so the test suite exercises real wire
behaviour on machines with no hardware attached.

## Response lengths: measured against the specification

All eight documented lengths were confirmed exactly. No discrepancies.

| Command | Spec | Spec section | Measured | Iterations |
|---|---|---|---|---|
| `<GETVER>>` | 14 | 1 | 14 | 420 |
| `<GETCPM>>` | 2 | 2 | 2 | 420 |
| `<GETVOLT>>` | 1 | 5 | 1 | 420 |
| `<GETCFG>>` | 256 | 7 | 256 | 120 |
| `<GETSERIAL>>` | 7 | 11 | 7 | 420 |
| `<GETDATETIME>>` | 7 | 23 | 7 | 420 |
| `<GETTEMP>>` | 4 | 24 | 4 | 418 of 420 |
| `<GETGYRO>>` | 7 | 25 | 7 | 420 |

The two `<GETTEMP>>` exceptions are covered under "Anomalies" below.

## Split reads

This is the central design constraint, and it is not an edge case.

**A response larger than 32 bytes always arrives split.** `<GETCFG>>` returned
its 256 bytes as exactly eight 32-byte reads on **120 of 120** attempts. That is
the CH340's bulk endpoint size, so the split is structural, not incidental:

```
+244.4ms n=32   +247.4ms n=32   +250.5ms n=32   +253.6ms n=32
+256.6ms n=32   +259.7ms n=32   +262.7ms n=32   +265.8ms n=32
```

Any implementation that issues one `read()` and trusts the result will get 32
bytes where it expected 256, every single time.

**Responses of 32 bytes or fewer usually arrive whole, but not always.** Across
420 iterations, `<GETVER>>` split into two reads once. The CH340 flushes its
buffer on an idle timer as well as when full, so a small response can straddle
that flush. At roughly 1 in 400 it is rare enough to survive testing and common
enough to happen several times a day at a 60-second poll interval.

The conclusion is the same for both cases: accumulate until the expected byte
count is reached or a deadline expires. Never trust a single read.

## Timing

| Measurement | Value |
|---|---|
| First-byte latency, minimum | 3.1 ms |
| First-byte latency, maximum | 171.3 ms |
| Full 256-byte `<GETCFG>>` | ~265 ms |

The 171 ms worst case sets the floor for any read deadline. This project uses a
2-second overall response deadline with a 50 ms per-read poll granularity, which
leaves a wide margin over the measured worst case while still failing fast
enough that a wedged device cannot stall a poll cycle.

## Anomalies

Both were observed during aggressive back-to-back polling, and both are
reproduced as test fixtures.

**A reply for a different command.** One `<GETTEMP>>` in 400 returned a full
256-byte configuration block instead of 4 bytes. Nothing else held the port. The
operational hazard is subtle: an implementation that reads only the 4 bytes it
expects would slice the front off that block and report a plausible but entirely
fabricated temperature. This is why `Exchange` performs a trailing-data check
after collecting a complete response and rejects the exchange as desynchronized
if the device is still talking.

**No reply at all.** One `<GETTEMP>>` in 400 returned zero bytes before the
deadline.

Both are recoverable conditions. The correct response to either is to log it,
skip that reading, and continue to the next poll.

**A single flipped bit.** This one was found later, during a live run rather
than a capture, and it is the most instructive of the three.

A device that had reported 30.8 °C on three consecutive polls returned 94.8 °C
on the fourth:

```
expected  1e 08 00 aa   ->  30.8 C
received  5e 08 00 aa   ->  94.8 C
```

`0x1E` against `0x5E` is one bit. Across 418 captured `<GETTEMP>>` replies the
integer byte was `0x1F` every single time, so this was line noise, not a
reading.

The lesson is that **structural validation is not sufficient**. The link runs
8N1 with no parity and the protocol carries no checksum, so a corrupted byte
produces a response of exactly the right length with exactly the right `0xAA`
terminator. Every framing check passes. Only a bound on the decoded value
rejects it.

This is why `ParseTemperature` and `ParseVoltage` apply plausibility ranges.
Voltage is the more exposed of the two: its reply is a single byte with no
terminator at all, so one flipped bit turns 4.2 V into 10.6 V with nothing
structural to notice.

The ranges are deliberately wide, chosen to reject corruption without
second-guessing a genuine extreme. GQ-RFC1201 documents no ranges, so these are
plausibility limits rather than specified ones.

## Calibration table: measured, not specified

**GQ-RFC1201 does not document the contents of the configuration block.**
Section 7 states only that `<GETCFG>>` returns 256 bytes. The CPM-to-dose
conversion had to be derived by measurement.

Method: 100 consecutive config blocks were captured and found byte-identical.
Every offset in the block was then tested as a candidate `uint16` followed by
`float32` pair. Exactly three offsets produced a sane count-rate to dose-rate
relationship, and all three agreed:

| Offset | CPM (`uint16`, big-endian) | µSv/h (`float32`, little-endian) | Ratio |
|---|---|---|---|
| 8 | 60 | 0.39 | 0.0065 |
| 14 | 240 | 1.56 | 0.0065 |
| 20 | 1000 | 6.50 | 0.0065 |

Three entries of 6 bytes each, beginning at offset 8.

**The endianness is mixed**, which is the detail most likely to be got wrong by
assumption: the CPM value is big-endian, consistent with `<GETCPM>>` in section
2, while the dose value is a little-endian IEEE-754 `float32`.

This was corroborated independently by black-box observation of the previously
deployed exporter's log output, which reported 28 CPM as 0.18 µSv/h. With the
table above, 28 × 0.0065 = 0.182. ✅

Because this layout is measured rather than specified, the conversion factor is
also exposed as configuration. A device with a different calibration, or a
firmware that moves these fields, can be handled without a code change.

## Device shutdown behaviour

Not a protocol finding, but it was measured on the host and it shapes a
requirement.

The previously deployed container exits with status **2** when sent `SIGTERM`.
Watchdog scripts commonly treat `0`, `143` and `137` as a clean stop, so an exit
of 2 is classified as a crash and the container gets restarted — including
during a maintenance window when it was deliberately stopped. That is what
interrupted one of the captures for this document.

This exporter therefore handles `SIGTERM` and `SIGINT` explicitly and exits
**0** on a clean shutdown, so a deliberate stop is distinguishable from a crash.
