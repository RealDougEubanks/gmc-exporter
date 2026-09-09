<!--
doc: CONTRIBUTING
last-refreshed: 2026-09-09
generated-by: doc-refresh skill
-->

# Contributing to gmc-exporter

## 1. Before you start

1. Read [README.md](./README.md) to understand what this does.
2. Read [docs/protocol-measurements.md](./docs/protocol-measurements.md) if you
   are touching anything under `internal/gmc/` or `internal/serialport/`. It
   records what the hardware actually does, and why the code is shaped that way.
3. Check the open issues so you do not duplicate work.
4. For anything large, open an issue first to agree the approach.

> **SECURITY:** Never commit secrets, API keys, tokens, passwords, or a real
> `config.ini`. They are effectively impossible to revoke once pushed, because
> git history persists in clones and forks. See [SECURITY.md](./SECURITY.md).

## 2. What you need

| Requirement | Version | Check with |
|---|---|---|
| Go | 1.25 or newer | `go version` |
| golangci-lint | v2 | `golangci-lint --version` |
| Docker | any recent | `docker --version` |
| A Geiger counter | optional | Not needed. See section 4 |

Install the linter:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

## 3. Workflow

> **SECURITY:** Never commit or push directly to `main`. Every change goes
> through a branch and a pull request.

1. Branch from `main`:

   ```bash
   git checkout main && git pull
   git checkout -b feature/short-description
   ```

   Use one of these prefixes: `feature/`, `fix/`, `hotfix/`.

2. Make your change.

3. Run every check that CI runs. All five must pass:

   ```bash
   gofmt -l .                  # must print nothing
   go vet ./...
   go build ./...
   go test -race ./...
   golangci-lint run
   ```

4. If you changed dependencies, tidy them. CI fails if this produces a diff:

   ```bash
   go mod tidy
   ```

5. Open a pull request with a title that says what changed and why.

## 4. Running the tests without hardware

**The full test suite passes on any machine with no Geiger counter attached.**
That is deliberate and worth preserving.

Protocol tests replay 1,860 byte sequences captured from a real GMC-320,
including the split reads, the malformed replies, and the two anomalies that
device produced. Fixtures live in `internal/gmc/testdata/captures/`.

```bash
go test -race ./...
```

### If you do have hardware

There is an opt-in integration test, skipped by default:

```bash
GOGMC_SERIAL=/dev/ttyUSB0 go test ./internal/gmc/ -run TestHardware -v
```

> The serial link is exclusive. Stop anything else using the device first, or
> two processes will interleave and produce output that looks exactly like a
> protocol bug.

### Re-recording fixtures

If the device disagrees with the code, **the device wins.** Re-record rather
than adjusting the test to match:

```bash
go build ./cmd/gmc-probe
sudo ./gmc-probe -device /dev/ttyUSB0 -iterations 20 -out capture.json
```

`gmc-probe` issues read-only commands only. It cannot alter the device.

## 5. Pull request checklist

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes
- [ ] `golangci-lint run` reports 0 issues
- [ ] `go mod tidy` produces no diff
- [ ] No new secrets or hardcoded credentials
- [ ] Docs updated if behaviour changed
- [ ] New logic has a test
- [ ] CI is green

For a shared or release branch, one approval from someone other than the author
is required. Solo self-merge is acceptable only when CI is green and the author
has reviewed the diff.

## 6. Code style

Run the linter. Configuration is in `.golangci.yml`. Beyond that:

| Rule | Reason |
|---|---|
| Comments explain **why**, not what | The code already says what |
| Errors are wrapped with context, never swallowed | An empty `catch` hides the failure |
| Ignore an error only as `_ = f()` | Makes the decision visible |
| Nothing in a poll cycle may terminate the process | This runs unattended for months |
| Every parser validates length before indexing | The most likely crash in this program |
| Credentials pass through `redact` before logging | Several APIs put passwords in URLs |

### Naming

Go conventions, per the table in [CLAUDE.md](./CLAUDE.md): `camelCase` for
unexported identifiers, `PascalCase` for exported, `snake_case` for filenames.

## 7. Adding a sink

Sinks are the most likely thing you will want to add. Each lives in its own
package under `internal/sink/`.

1. Read `internal/sink/sink.go` for the interface.
2. Create `internal/sink/yourbackend/`.
3. Implement `Name()`, `Publish(ctx, reading.Reading) error`, and `Close()`.
4. Add configuration to `internal/config/config.go`, disabled by default.
5. Wire it into `buildSinks` in `cmd/gmc-exporter/main.go`.
6. Document every setting in [docs/ENV_VARS.md](./docs/ENV_VARS.md) and
   `.env.example`.

Requirements for every sink:

| Requirement | Why |
|---|---|
| Disabled by default | Nobody should publish data they did not ask to publish |
| Never panics, never calls `os.Exit` | One backend must not kill the process |
| Bounded retry with backoff, honouring `ctx` | Hammering a backend is not resilience |
| Never retries an auth failure | Wrong credentials stay wrong |
| Every HTTP error through `redact.Error` | Otherwise a URL leaks a password |
| Verifies the response body, not just the status | Some APIs return 200 with an error in the body |
| Omits optional values that are absent | A defaulted zero is indistinguishable from a real reading |

> **SECURITY:** Your sink's tests must include a canary test: put a distinctive
> fake credential in the config, force a transport failure, capture `slog`
> output, and assert the credential appears in neither the log nor the returned
> error. Every existing sink has one. Copy the pattern from
> `internal/sink/radmon/radmon_test.go`.

Never let a test send data to a real public dataset. Use `httptest`.

## 8. Releasing

Releases are tag-driven. Pushing a `v*` tag builds multi-architecture images and
pushes them to Docker Hub.

```bash
git tag -a v0.4.0 -m "v0.4.0 — what changed"
git push origin v0.4.0
```

Use semantic versioning: patch for a fix, minor for a new feature, major for a
breaking change.

> **SECURITY:** The release workflow authenticates with a scoped Docker Hub
> access token in the `DOCKERHUB_TOKEN` repository secret. Never use an account
> password. A token can be scoped to write-only and revoked on its own.

## 9. Related documents

| Document | Use it for |
|---|---|
| [README.md](./README.md) | What this is and how to run it |
| [SECURITY.md](./SECURITY.md) | Credential rules, reporting a vulnerability |
| [docs/ENV_VARS.md](./docs/ENV_VARS.md) | Every setting |
| [docs/RUNBOOK.md](./docs/RUNBOOK.md) | Operating it in production |
| [docs/protocol-measurements.md](./docs/protocol-measurements.md) | Hardware behaviour and evidence |
