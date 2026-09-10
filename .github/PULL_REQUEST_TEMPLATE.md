## What this changes, and why

<!-- The why matters more than the what; the diff already says what. -->

## Checklist

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes
- [ ] `golangci-lint run` reports 0 issues
- [ ] `go mod tidy` produces no diff
- [ ] No new secrets or hardcoded credentials
- [ ] New logic has a test
- [ ] Docs updated if behaviour changed

## If this touches a sink

- [ ] Disabled by default
- [ ] Cannot panic or call `os.Exit`
- [ ] Bounded retry with backoff, honouring `ctx`
- [ ] Auth failures are not retried
- [ ] Every HTTP error passes through `redact.Error`
- [ ] Response body is verified, not just the status code
- [ ] A canary test asserts no credential reaches the log or the returned error

## If this touches the protocol layer

- [ ] Length validated before indexing
- [ ] Claims about device behaviour trace to a capture, to GQ-RFC1201, or to a
      measurement — say which
