# Turn-State Cache Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make token publication, collection, management, logging, lifecycle, and release behavior fail closed and remain bounded.

**Architecture:** Keep the CPA-native plugin and in-memory cache. Introduce one strict candidate parser shared by all capture paths, richer collector scheduling state, explicit value reveal, serialized runtime replacement, and CI-backed ABI verification.

**Tech Stack:** Go 1.26, cgo CPA plugin ABI, embedded HTML/JavaScript, JSONL, Docker, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-09-18-turn-state-hardening-design.md`

## Global Constraints

- Add no dependencies.
- Keep turn-state values memory-only.
- Preserve account+model request binding and both injection modes.
- Preserve Linux/amd64 CPA ABI compatibility.
- Write a failing regression test before each production behavior change.

---

### Task 1: Strict monotonic state publication

**Files:**
- Modify: `internal/turnstate/fernet.go`
- Modify: `internal/turnstate/cache.go`
- Test: `internal/turnstate/cache_test.go`

**Interfaces:**
- Produces: strict `ParseFernet(value string) (issuedAt time.Time, fingerprint string, ok bool)`.
- Produces: `StoreStale` outcome for an older or expired candidate.

- [ ] Add failing tests proving malformed 292-byte values, expired values, and older out-of-order responses are rejected.
- [ ] Run `go test ./internal/turnstate -run 'TestCache(RejectsMalformed|RejectsExpired|DoesNotRegress)' -count=1` and confirm failure.
- [ ] Implement strict envelope parsing, a 30-second expiry margin, and monotonic replacement.
- [ ] Run the focused tests and the complete `internal/turnstate` package.

### Task 2: Probe completion and retry policy

**Files:**
- Modify: `cmd/plugin/collector.go`
- Modify: `cmd/plugin/collector_test.go`
- Modify: `cmd/plugin/management.go`
- Test: `cmd/plugin/main_test.go`

**Interfaces:**
- Produces: `collectorProbeResult.RetryAfter time.Duration` and accepted-store semantics.
- Produces: scheduler state for blocked targets, failure count, and next retry.

- [ ] Add failing tests for non-2xx-with-header, missing `response.completed`, bounded bodies, Retry-After, 401/403 blocking, and manual probe cache rejection.
- [ ] Run those focused tests and confirm each fails for the missing behavior.
- [ ] Require 2xx plus completed bounded SSE before harvesting; parse Retry-After.
- [ ] Add status-visible blocked/backoff state and only report harvested after `StoreForBucket` accepts.
- [ ] Run `go test ./cmd/plugin -count=1`.

### Task 3: Collector transport safety

**Files:**
- Modify: `cmd/plugin/collector.go`
- Test: `cmd/plugin/collector_test.go`

**Interfaces:**
- Consumes: existing `collectorProbeOptions`.
- Produces: context-bounded SOCKS5 negotiation and HTTPS endpoint validation.

- [ ] Add failing tests for a SOCKS server that accepts but never greets and for a credential HTTP base URL.
- [ ] Confirm the cancellation and insecure-endpoint tests fail.
- [ ] Set/clear connection deadlines, close on context cancellation, and validate credential endpoints.
- [ ] Run focused tests with `-race`.

### Task 4: Management privacy contract

**Files:**
- Modify: `internal/turnstate/cache.go`
- Modify: `internal/turnstate/interceptor.go`
- Modify: `cmd/plugin/management.go`
- Modify: `cmd/plugin/dashboard.html`
- Test: `cmd/plugin/main_test.go`
- Test: `internal/turnstate/cache_test.go`

**Interfaces:**
- Produces: value-free `BucketStatus` with `fingerprint` and `preview`.
- Produces: authenticated POST `/buckets/reveal` response containing one value.

- [ ] Add failing tests proving status JSON contains no full value and reveal returns one value with `no-store`.
- [ ] Add a failing dashboard contract test proving full values are fetched only on explicit copy.
- [ ] Remove full values from snapshots, add a direct cache lookup for reveal, and add the management route.
- [ ] Keep the key only in JavaScript memory and update the dashboard copy flow and safety copy.
- [ ] Run package tests.

### Task 5: Log identity and failure hardening

**Files:**
- Modify: `cmd/plugin/collector_log.go`
- Modify: `cmd/plugin/main.go`
- Test: `cmd/plugin/collector_test.go`
- Modify: `.gitignore`

**Interfaces:**
- Produces: stable `accountAlias(authID string) string` used by persistent and host logs.
- Produces: malformed-record count in `collectorLogStatus`.

- [ ] Add failing tests proving raw account IDs and proxy errors are absent from persisted and host-log payloads.
- [ ] Add failing tests for malformed JSONL, partial trailing records, symlinks, and records crossing 64 KiB blocks.
- [ ] Implement aliases, private directory enforcement, symlink refusal, and malformed-record reporting.
- [ ] Move discovery log persistence off the interception call stack with a bounded queue and shutdown flush.
- [ ] Run focused tests and race tests.

### Task 6: Atomic reconfiguration and scalable snapshots

**Files:**
- Modify: `cmd/plugin/main.go`
- Modify: `internal/turnstate/cache.go`
- Test: `cmd/plugin/main_test.go`
- Test: `internal/turnstate/cache_test.go`

**Interfaces:**
- Produces: one runtime generation swapped under a lifecycle mutex.
- Produces: strict YAML decoding and lock-minimized snapshot sorting.

- [ ] Add failing tests for unknown YAML fields, concurrent configure, cache preservation on collector-only changes, and deterministic large snapshots.
- [ ] Confirm expected failures.
- [ ] Serialize configure, decode YAML with `KnownFields(true)`, preserve compatible cache/plugin state, and sort copied buckets outside the cache lock.
- [ ] Run package and race tests.

### Task 7: CI, release consistency, and documentation

**Files:**
- Modify: `cmd/plugin/main.go`
- Modify: `scripts/build-linux-amd64.sh`
- Modify: `scripts/build_script_test.go`
- Create: `.github/workflows/ci.yml`
- Modify: `README.md`
- Modify: `docs/PRD.md`
- Update: `docs/dashboard-preview.png`

**Interfaces:**
- Produces: build-injected `main.version` and persisted SHA256 checksum.

- [ ] Add failing build-script tests for invalid/mismatched versions and checksum output.
- [ ] Inject version with ldflags and make the script run test/vet before producing the artifact.
- [ ] Add CI for format, test, race, vet, and read-only Docker ABI smoke.
- [ ] Update stale architecture comparison, privacy wording, and dashboard screenshot.
- [ ] Run all verification commands and inspect the final diff.

### Task 8: Final verification

**Files:** none.

- [ ] Run `gofmt -l cmd internal scripts` and require no output.
- [ ] Run `git diff --check`.
- [ ] Run `go test -count=1 ./...`.
- [ ] Run `go test -race -count=1 ./...`.
- [ ] Run `go vet ./...`.
- [ ] Build in a read-only mounted Linux/amd64 container and require `ABI_SMOKE_OK`, ELF64/DYN/x86-64, and exported `cliproxy_plugin_init`.
