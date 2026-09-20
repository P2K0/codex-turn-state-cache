# Dashboard Collector Settings Design

## Goal

Allow an operator to configure the built-in collector from the plugin dashboard and persist the values in CPA's existing `config.yaml`.

## Architecture

The dashboard uses CPA's authenticated `PATCH /v0/management/plugins/cpa-plugin-codex-turn-state/config` endpoint. CPA already shallow-merges the plugin object, writes the main configuration file, and reloads plugin configuration, so the plugin does not own a second configuration file.

The plugin status response remains credential-safe. It reports whether a collector proxy exists and the active refresh/retry values, but never returns the proxy URL or embedded username/password. The settings form therefore leaves the proxy input blank when an existing proxy is configured; a blank value preserves it, a new value replaces it, and an explicit clear action removes it.

## Interface

- Add a `采集设置` button to the dashboard toolbar.
- Open an inline settings card containing:
  - dedicated proxy URL (password-style input, never prefilled),
  - refresh-before seconds,
  - retry seconds,
  - collector enabled/configured state and discovered target count,
  - save, clear-proxy, and close actions.
- Add a collector badge to the existing status badges.
- Keep demo mode side-effect free while allowing the settings interaction to be previewed.

## Validation And Errors

- Accept only `http`, `https`, `socks5`, and `socks5h` URLs with a hostname.
- Require refresh-before to be an integer from 1 through 3599.
- Require retry seconds to be a positive integer.
- Reuse the dashboard management-key flow for authenticated configuration updates.
- Keep the form open and report an inline error when validation or persistence fails.

## Verification

- Static dashboard tests lock the settings controls, supported fields, authenticated CPA config endpoint, secret-preserving behavior, and explicit clear operation.
- Existing Go tests, `go vet`, race tests, and Docker ABI build remain green.
- Browser verification opens the settings panel in demo mode and captures the resulting layout.

