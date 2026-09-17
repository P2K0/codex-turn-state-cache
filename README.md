# CPA Codex Turn State Cache

Native CLIProxyAPI v7.3.4 plugin for reusing a qualifying
`X-Codex-Turn-State` response header on later Codex upstream requests.

## Behavior

- The plugin accepts exactly one response header value with raw byte length `292`.
- The cache key is the CPA-selected `selected_auth_id` plus the exact after-auth
  model string. Client IP and forwarded IP are not part of the key.
- A later valid value for the same key replaces the previous value and sets a
  new fixed one-hour expiration.
- Invalid values, missing request correlation, a different auth record, or a
  different model do not create or replace an entry.
- On a cache hit, the plugin returns one replacement
  `X-Codex-Turn-State` request header. On a miss, it returns no header change.
- State is process-local only and is discarded on reconfiguration, plugin
  quiesce, or CPA restart.

HTTP responses and HTTP stream header-initialization events are supported.
WebSocket response payloads are intentionally out of scope.

## CPA Configuration

Place the Linux/amd64 shared library at:

```text
/CLIProxyAPI/plugins/linux/amd64/codex-turn-state-cache-v0.1.0.so
```

Enable it in the CPA configuration:

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

`max_entries` and `max_pending_entries` are optional. The 292-byte validation
and one-hour TTL are fixed by the plugin.

## Build

The target CPA runtime is Linux/amd64. Build the shared library locally or in
CI, not on the production server.

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
  -o dist/codex-turn-state-cache-v0.1.0.so ./cmd/plugin
```

Verify the required native entry point before deployment:

```powershell
go tool nm dist/codex-turn-state-cache-v0.1.0.so |
  Select-String 'cliproxy_plugin_init'
```

## Rollback

Disable the `codex-turn-state-cache` plugin configuration and remove its
read-only plugin-directory mount, then recreate only the CPA application
service. Do not change the tunnel, published port, or authentication directory.
