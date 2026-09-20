# Turn-State Cache Hardening Design

## Goal

Harden the existing CPA-native turn-state cache without changing its core
deployment model: no extra daemon, no interception of business bodies, no
persistent turn-state values, and no new dependencies.

## Scope

The work covers four connected boundaries:

1. State acceptance and publication must be monotonic and fail closed.
2. Collector probes must respect upstream completion and retry signals.
3. Management and audit surfaces must not expose more sensitive data than
   their documented contract.
4. Reconfiguration, logging, status rendering, and release verification must
   remain bounded under failure and scale.

## State model

All capture paths use the same strict Fernet-envelope parser. A candidate is
accepted only when it is a strict URL-safe base64 token with version `0x80`,
the expected 217 decoded bytes / 292 encoded bytes, a plausible timestamp,
and at least 30 seconds of remaining lifetime. The cache never replaces an
entry with a candidate whose issuance time is older than the current entry.
Equal-time candidates may replace only when their value differs, preserving
compatibility with upstream re-signing while preventing expiry regression.

Store outcomes distinguish invalid, stale, new, and replacement candidates.
Automatic and manual probe responses report success only when the cache
accepted the candidate.

## Collector policy

A successful probe requires a 2xx response, a bounded response body, and a
`response.completed` SSE event. The collector parses `Retry-After`, blocks
401/403 targets until the credential is observed again, and applies bounded
exponential backoff with jitter-free deterministic tests for other failures.
The status surface reports a target's failure state and next retry time.

SOCKS5 negotiation is covered by the request context: cancellation closes the
socket and all handshake operations use the context deadline. Credential
`base_url` values must be HTTPS, except loopback HTTP used by tests.

## Privacy and management

The normal status response contains a short fingerprint and preview, never the
full turn-state. A separate authenticated POST endpoint reveals one bucket on
explicit operator action and responds with `Cache-Control: no-store`. The
dashboard keeps the management key in memory only and requests a value only
when the copy control is used.

Persistent collector logs store a stable one-way account alias rather than the
raw credential identifier. The in-memory management view may continue to use
the raw identifier because it is already inside the authenticated CPA control
plane. Host logs use the alias as well.

## Durable logging

Event production does not fsync on the request interception path. A bounded
queue feeds one writer goroutine; overflow is counted rather than blocking
business traffic. The file remains append-only from the plugin's perspective,
but is segmented at a configurable internal size so complete history can be
archived without one unbounded file. The reader reports malformed records and
repairs or separates a trailing partial record before appending.

For this delivery, segmentation remains conservative: preserve the current
file and pagination contract, add a maximum accepted record size, malformed
record accounting, symlink refusal, private directory permissions, and make
the hot-path discovery event asynchronous. Full archival/export is a later
operational feature rather than an implicit deletion policy.

## Lifecycle and scale

Plugin reconfiguration is serialized by a lifecycle mutex. A fully prepared
runtime generation is swapped atomically; unchanged cache state is retained
when compatible. Unknown YAML fields fail configuration. Snapshots copy under
lock and sort outside it using `sort.Slice`; management status supports bounded
bucket results.

## Build and verification

One version source is injected into the c-shared binary at build time. CI runs
format checks, tests, race tests, vet, and the Linux/amd64 Docker ABI smoke.
The release artifact receives a persisted checksum. Documentation and the
dashboard preview must match the shipped behavior.

## Non-goals

- No on-disk turn-state persistence.
- No standalone reverse proxy or Codex config takeover.
- No new proxy library or other dependency.
- No claim that Fernet shape proves model quality or token authenticity.
