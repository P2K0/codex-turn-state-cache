# Codex Turn-State Cache

Native CLIProxyAPI plugin that reuses a qualifying upstream
`X-Codex-Turn-State` header on later Codex requests — the 292-state
anti-degradation mechanism, fully automated.

## How it works

Upstream Codex responses carry an `X-Codex-Turn-State` header whose value is
a Fernet token. Two value lengths exist:

| State | base64 length | Meaning |
| --- | ---: | --- |
| normal | 292 | a reusable "template" — the good-serving-state pass |
| degraded | 312 | throttle/degradation marker, **never reusable** |

The plugin keeps a per `(account, model)` in-memory cache of the latest 292
template and injects it on later requests, which keeps Codex out of the
degraded/overloaded serving path. Without collector configuration it is fully
passive: every successful response refreshes the template. When a dedicated
collector proxy is configured, the same plugin also refreshes discovered
account/model buckets automatically — there is still no keeper daemon or
external collection process.

### Rules (enforced in code)

1. A state value is **never** reused across accounts.
2. A state value is **never** reused across models, even within one account.
3. Reuse within one `(account, model)` bucket is allowed **across client or
   egress IP changes**.
4. A template expires after one hour, measured **from the token's own embedded
   Fernet timestamp**, not from when the proxy captured it. A timestamp in the
   future is rejected outright.

### Behavior

- Accepts exactly one upstream header value whose raw byte length is `292`
  (template) or `312` (degraded — counted and logged, never stored).
- Keys state by CPA's selected account and the exact after-auth model.
- Replaces a prior valid value for the same key. Reads do not extend the
  lifetime.
- Captures HTTP responses and stream header initialization. A completed
  request cannot later populate the cache.
- Keeps state only in process memory. Collector-only reconfiguration preserves
  compatible buckets; capacity changes, quiesce, unload, and CPA restart clear
  them — the next successful response rebuilds the cache.

### inject_mode

- `always` (default): inject the live template on every request for the
  bucket (the original v0.1 behavior).
- `replace-only`: rewrite **only** requests already carrying a degraded
  312-length state. Headerless requests are never force-fed a template.
  Any other value is rejected at startup rather than silently treated as
  `always`.

## Management API & dashboard

The plugin registers a dashboard page in the CPAMP menu
(**Codex Turn-State Cache**) and five authenticated data routes:

| Route | Prefix | Auth |
| --- | --- | --- |
| `/dashboard` (menu: Codex Turn-State Cache) | `/v0/resource/plugins/cpa-plugin-codex-turn-state/` | no (zero-data shell) |
| `GET /cpa-plugin-codex-turn-state/status` | `/v0/management/` | yes |
| `POST /cpa-plugin-codex-turn-state/cache/clear` | `/v0/management/` | yes |
| `POST /cpa-plugin-codex-turn-state/buckets/probe` | `/v0/management/` | yes |
| `POST /cpa-plugin-codex-turn-state/buckets/delete` | `/v0/management/` | yes |
| `POST /cpa-plugin-codex-turn-state/buckets/reveal` | `/v0/management/` | yes, `no-store` |

The shell is pure HTML + JS with no data; the browser fetches metadata from
the authenticated status route with the management key held only in page
memory. Status never contains a state value. An explicit copy action calls
the authenticated `buckets/reveal` route, which returns the value with
`Cache-Control: no-store`, then copies it without persisting it in the DOM.
Append `#demo` to the dashboard URL to preview it with built-in sample data.

Dashboard columns: account (level 1) with the credential's subscription tier
(`chatgpt_plan_type` from the auth file's JWT), model rows (level 2) with
status, remaining lifetime, state length (click to copy on demand),
issue/expiry times, and per-bucket actions — **探测** (issue one minimal
upstream request locked to that account+model via `host.model.execute` and
harvest the fresh 292 straight from the returned headers) and **删除** (drop
that cached template; traffic rebuilds it).

## Logging

Collector events are appended as JSON Lines to
`data/codex-turn-state-cache/collector-events.jsonl`. The plugin never rotates,
truncates, or automatically deletes this file, so the complete history remains
available across CPA restarts. Mount `/CLIProxyAPI/data` as persistent storage
when CPA itself runs in a replaceable container. The dashboard reads only the
latest 50 entries for responsiveness; this does not remove older file entries.

Each collector event records its timestamp, phase, source, account ID, model, HTTP status,
state length, retry delay, and a sanitized result. It never records access
tokens, proxy URLs/passwords, request headers/bodies, or state
values. The same sanitized event summary is also sent through CPA's native
`host.log` callback.

The persistent collector phases are:

```text
target_discovered
collection_started
collection_succeeded
collection_failed (including the next retry delay)
collector_configured
```

Normal cache capture/injection decisions continue to use CPA's `host.log`
callback without writing sensitive request or state data.

## CPA Configuration

Install from the CPA plugin store, or place the Linux/amd64 library at:

```text
/CLIProxyAPI/plugins/linux/amd64/cpa-plugin-codex-turn-state-v0.3.0.so
```

Then enable it in CPA configuration:

```yaml
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    cpa-plugin-codex-turn-state:
      enabled: true
      priority: 0
      max_entries: 10000
      max_pending_entries: 20000
      inject_mode: replace-only   # or "always" (default)
      collector_proxy_url: http://user:pass@192.168.1.10:7890
      collector_refresh_before_seconds: 300
      collector_retry_seconds: 60
```

All fields are optional. `collector_proxy_url` supports `http`, `https`,
`socks5`, and `socks5h`. When it is set, the plugin remembers every
`(account, model)` observed after CPA selects a credential, immediately
collects a missing template through that proxy, and refreshes it before
expiry. The collector reads the selected credential through CPA's host
callback and sends one minimal `store: false` request directly; normal
business requests continue using CPA's existing per-account or global route.
Failed or empty collections retry after `collector_retry_seconds`. Manual
**探测** uses the same dedicated route.

The dashboard's **采集设置** panel can persist the same values without editing
YAML by hand. It uses CPA's authenticated plugin-config endpoint, which writes
`config.yaml` and reloads the plugin. Saved proxy credentials are never read
back or rendered by the dashboard: leave the proxy field empty to keep the
current value, enter a complete URL to replace it, or use **清除代理** to remove
it explicitly.

The state lengths and the one-hour token lifetime are fixed.

## Build

Build the Linux/amd64 shared library locally or in CI, not on the CPA host.

With Docker:

```bash
./scripts/build-linux-amd64.sh
```

Override the default public Go image or artifact version when needed:

```bash
BUILD_IMAGE=golang:1.26-bookworm VERSION=0.3.0 ./scripts/build-linux-amd64.sh
```

```powershell
$zig = 'C:\path\to\zig.exe'
$env:CGO_ENABLED = '1'
$env:CC = "$zig cc"
go test ./...
go vet ./...

$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
$env:CC = "$zig cc -target x86_64-linux-gnu.2.17"
go build -trimpath -buildmode=c-shared `
  -o dist/cpa-plugin-codex-turn-state-v0.3.0.so ./cmd/plugin
```

On macOS with Homebrew Go + Zig:

```bash
export PATH=/opt/homebrew/bin:$PATH
go test ./... && go vet ./...
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC="zig cc -target x86_64-linux-gnu.2.17" \
  go build -trimpath -buildmode=c-shared \
  -o dist/cpa-plugin-codex-turn-state-v0.3.0.so ./cmd/plugin
```

The ABI can be verified against a real Linux loader with
`native_abi_smoke.c` (see file header for usage).

## Release Asset

The GitHub Release supports Linux/amd64 only. Its zip asset is named
`cpa-plugin-codex-turn-state_0.3.0_linux_amd64.zip`, contains only
`codex-turn-state-cache.so` at the archive root, and is verified by the
adjacent `checksums.txt` file.

## GitHub Actions

Every push to `main` and every pull request runs the Go tests, `go vet`,
Linux/amd64 compilation, and the native ABI smoke test. To publish a release,
push a tag whose version matches `pluginVersion` in `cmd/plugin/main.go`:

```bash
git tag v0.3.0
git push origin v0.3.0
```

The tag workflow builds the plugin, packages the release zip and
`checksums.txt`, uploads the build artifact, and creates a GitHub Release with
generated notes.

## Design notes

- Design comparison against the article's keeper-based architecture and the
  probe/business-role variant lives in [docs/PRD.md](docs/PRD.md).
- Why the Fernet timestamp is the expiry basis, and why a future timestamp is
  rejected: see the "TTL 基准" section of docs/PRD.md.
