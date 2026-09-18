# Codex Turn-State Cache

Native CLIProxyAPI plugin that reuses a qualifying upstream
`X-Codex-Turn-State` header on later Codex requests.

## Behavior

- Accepts exactly one upstream header value whose raw byte length is `292`.
- Keys state by CPA's selected account and the exact after-auth model.
- Reuses a matching account/model state across client or egress IP changes.
- Replaces a prior valid value for the same key and gives the new value a fixed
  one-hour lifetime. Reads do not extend that lifetime.
- Captures HTTP responses and stream header initialization. A completed request
  cannot later populate the cache.
- Keeps state only in process memory. Reconfiguration, quiesce, unload, and
  CPA restart clear it.

Successful captures and injections use CPA's native `host.log` callback. Logs
include the plugin ID, model, and one of the following messages, but never a
state value, account ID, IP, request header, or body:

```text
codex turn-state cache captured source=http
codex turn-state cache captured source=stream
codex turn-state cache captured source=<http|stream> replaced=true
codex turn-state cache injected
```

## CPA Configuration

Install from the CPA plugin store, or place the Linux/amd64 library at:

```text
/CLIProxyAPI/plugins/linux/amd64/codex-turn-state-cache-v0.1.2.so
```

Then enable it in CPA configuration:

```yaml
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    codex-turn-state-cache:
      enabled: true
      priority: 0
      max_entries: 10000
      max_pending_entries: 20000
```

`max_entries` and `max_pending_entries` are optional. The state length and
one-hour lifetime are fixed.

## Build

Build the Linux/amd64 shared library locally or in CI, not on the CPA host.

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
  -o dist/codex-turn-state-cache-v0.1.2.so ./cmd/plugin
```

## Release Asset

The GitHub Release supports Linux/amd64 only. Its zip asset is named
`codex-turn-state-cache_0.1.2_linux_amd64.zip`, contains only
`codex-turn-state-cache.so` at the archive root, and is verified by the
adjacent `checksums.txt` file.

## Related Links

- [LINUX DO](https://linux.do/) — 新的理想型社区
