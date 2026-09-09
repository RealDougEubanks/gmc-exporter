<!--
doc: SECURITY
last-refreshed: 2026-09-09
generated-by: doc-refresh skill
-->

# Security Policy

## 1. Reporting a vulnerability

> **SECURITY: Do NOT open a public GitHub issue for a security vulnerability.**
> A public issue tells everyone about the problem before there is a fix.

Report privately using GitHub Security Advisories:

<https://github.com/RealDougEubanks/gmc-exporter/security/advisories/new>

Expected acknowledgement: **within 48 hours.**

Please include:

1. What the vulnerability allows an attacker to do.
2. The steps to reproduce it.
3. The version affected. Get it with `gmc-exporter --version`.

> **SECURITY:** Do not attach raw logs or configuration to a report until you
> have searched them for your own credentials. Search for each of your passwords
> and tokens by value before sharing any file.

## 2. Sensitive data this project handles

This exporter reads radiation measurements. It holds no personal data and no
payment data. It does handle two categories that matter.

### 2.1 Credentials for other services

| Credential | Used for | Where it goes |
|---|---|---|
| radmon.org data-sending password | Submitting readings | In the request URL. See 4.1 |
| InfluxDB 1.x username and password | Writing points | In the request URL |
| InfluxDB 2.x API token | Writing points | `Authorization: Token` header |
| MQTT username and password | Broker authentication | MQTT CONNECT packet |
| OTLP headers | Collector authentication | gRPC or HTTP headers |
| GMCMAP account and counter ID | Submitting readings | In the request URL |
| Safecast API key | Submitting readings | In the request URL |

### 2.2 Your physical location

> **SECURITY:** `GOGMC_LATITUDE` and `GOGMC_LONGITUDE` are **published
> publicly** and cannot be recalled. They are attached to every reading sent to
> radmon.org, GMCMAP, or Safecast. A Geiger counter is usually at the operator's
> home, so these coordinates are effectively a home address.

Before enabling any public map sink, decide how precise you are willing to be.
Two decimal places is roughly a kilometre. Full precision is your building.

Sinks that require coordinates refuse to start without them rather than sending
a default, because a default would silently attribute your readings to a place
you are not.

## 3. Credential and secret rules

> **SECURITY:** Never commit a secret. Git history is effectively permanent and
> rewriting it does not remove the value from clones, forks, or caches. If a
> secret is committed, rotate it at the issuing service. Deleting the commit is
> not enough.

1. **Prefer file-based secrets.** Every secret setting accepts a `_FILE`
   variant:

   ```bash
   GOGMC_RADMON_PASSWORD_FILE=/run/secrets/radmon_password
   ```

   A plain environment variable is readable by anyone who can run
   `docker inspect`, and is inherited by every child process.

2. **Never commit a real config file.** `.gitignore` already excludes `.env`,
   `config.ini`, `*.pem` and `*.key`. Only `.env.example` belongs in the repo.

3. **Rotate at the source.** Changing a value here does not invalidate the old
   one. Rotate it at radmon.org, InfluxDB, your broker, or Safecast.

4. **Use least privilege.** For InfluxDB 2.x, issue a token that can write to
   one bucket. Do not use an all-access token.

See [docs/ENV_VARS.md](./docs/ENV_VARS.md) for where to obtain each credential.

## 4. Security controls in this project

These are implemented and tested, not aspirational.

### 4.1 Credentials never reach the logs

Several of the APIs this exporter talks to accept credentials only as URL query
parameters, so a request URL contains a password. Go's `net/http` returns
transport failures as `*url.Error`, which embeds the full URL. Logging such an
error directly writes a plaintext password to disk.

Controls, in `internal/redact/redact.go`:

| Control | What it prevents |
|---|---|
| `redact.Error` strips credentials from `*url.Error` | A transport failure logging the password |
| Response excerpts are scrubbed before truncation | A server that echoes the request back |
| `redact.URL` keeps parameter names, replaces values | A URL in an error message leaking a value |
| `redact.Secret` holds its value in a closure | Reflection printing it. See below |

`redact.Secret` deliberately stores its value in a function, not a string.
Implementing `Stringer` is not sufficient: `fmt` walks unexported struct fields
by reflection and cannot call methods on what it finds there. With a plain
string type, logging a whole sink struct prints the password:

```
fmt.Sprintf("%v", sink)  ->  {radmon.org someuser hunter2}
```

A function field has no readable contents, so reflection can only print its
address. `TestSecretSurvivesReflection` in
`internal/redact/redact_test.go` fails if anyone simplifies this back to a
string.

To be accurate about the threat: over HTTPS a query string is encrypted in
transit. The risk is **log hygiene**, not interception. Logs get pasted into
tickets and shipped to aggregators.

### 4.2 The container does not run as root

The image is built `FROM gcr.io/distroless/static-debian12:nonroot`. There is no
shell, no package manager, and no libc. It runs as uid 65532.

> **SECURITY:** Do not use `--privileged` to fix a device permission error. It
> grants every capability on the host in order to solve a file-permission
> problem. Use `--device` with `--group-add` instead:
>
> ```bash
> docker run --device=/dev/ttyUSB0 --group-add="$(stat -c '%g' /dev/ttyUSB0)" ...
> ```

Measured behaviour, since these are easy to conflate:

| Device passed as | `--privileged` | `--group-add` | Works |
|---|---|---|---|
| `--device` | no | yes | yes |
| volume mount | yes | yes | yes |
| volume mount | yes | no | no |
| volume mount | no | yes | no |

A volume mount makes the node visible but grants no device permission. That is
why it still needs `--privileged`.

### 4.3 Input from the device is treated as untrusted

The serial link runs 8N1 with no parity and the protocol carries no checksum, so
a corrupted byte is undetectable structurally.

| Control | Location |
|---|---|
| Every parser checks length before indexing | `internal/gmc/parse.go` |
| Responses longer than expected are rejected, not truncated | `internal/gmc/protocol.go` |
| Decoded values are range-checked for plausibility | `internal/gmc/parse.go` |
| Response bodies are size-capped before reading | each sink |
| Each poll cycle is wrapped in `recover()` | `internal/poller/poller.go` |

This is not theoretical. A live run produced a reading of 94.8 °C from a device
sitting at 30.8 °C, caused by one flipped bit. It had the correct length and the
correct terminator byte, so only a value bound rejected it.

### 4.4 The health endpoints do not leak infrastructure

`/health` is unauthenticated so external monitors can reach it. It reports which
sinks are configured, but no URLs, hostnames, connection strings, or tokens.

> **SECURITY:** Do not add connection details to the health response. An
> unauthenticated endpoint that lists your internal hostnames is a
> reconnaissance tool.

### 4.5 Supply chain

| Control | Detail |
|---|---|
| Release provenance | Build provenance attestation, pushed to the registry |
| SBOM | Generated and attached at release |
| Dependency updates | Dependabot, weekly, for Go modules, Actions, and Docker |
| Pinned base image | `gcr.io/distroless/static-debian12:nonroot` |
| Registry auth | A scoped Docker Hub access token, never an account password |
| Static binary | `CGO_ENABLED=0`, so no dynamic libraries in the final image |

## 5. Dependency security

Run this before every release:

```bash
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...
```

> **Known gap:** `govulncheck` is not yet part of the CI workflow. It must be run
> manually. See [docs/assumptions.md](./docs/assumptions.md).

The linter also runs on every push and pull request, configured in
`.golangci.yml`, including `bodyclose` and `noctx`, which catch the resource
leaks that accumulate in long-running processes.

## 6. Reducing exposure

The most secure configuration enables only the Prometheus sink.

```bash
GOGMC_PROMETHEUS_ENABLED=true
```

It needs **no credentials at all**. Your monitoring system pulls from it, so
there is nothing to leak, nothing to rotate, and no outbound network access.
Every other sink pushes data somewhere and requires a credential to do so.

## 7. Related documents

| Document | Use it for |
|---|---|
| [docs/ENV_VARS.md](./docs/ENV_VARS.md) | Every setting, and where to get each secret |
| [docs/RUNBOOK.md](./docs/RUNBOOK.md) | Incident response steps |
| [CONTRIBUTING.md](./CONTRIBUTING.md) | Rules for code changes |
