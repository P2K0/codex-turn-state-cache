# Dashboard Collector Settings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a secure, persistent collector-proxy settings panel to the plugin dashboard.

**Architecture:** The dashboard writes the existing CPA plugin config endpoint instead of introducing plugin-owned persistence. The status endpoint remains the only read source and never exposes the saved proxy URL, while CPA persists the patch and reloads the plugin.

**Tech Stack:** Embedded HTML/CSS/JavaScript, Go static contract tests, CPA management API.

---

### Task 1: Lock the dashboard contract

**Files:**
- Modify: `cmd/plugin/main_test.go`

- [ ] **Step 1: Add a failing static dashboard test**

Assert that the embedded page contains the settings button/card, password input, numeric inputs, collector badge rendering, the CPA plugin-config PATCH path, supported proxy scheme validation, omission of a blank proxy from save payloads, and an explicit `null` clear payload.

- [ ] **Step 2: Run the focused test and verify RED**

Run: `go test ./cmd/plugin -run TestDashboardOffersPersistentCollectorSettings -count=1`

Expected: FAIL because the dashboard does not yet contain collector settings controls.

### Task 2: Implement persistent settings UI

**Files:**
- Modify: `cmd/plugin/dashboard.html`

- [ ] **Step 1: Add compact settings styles and markup**

Add a toolbar button and hidden inline card with a responsive field grid, password-style proxy input, two numeric inputs, status summary, and save/clear/close actions.

- [ ] **Step 2: Add settings state and collector badge rendering**

Keep the latest status document in page state, populate only non-secret numeric fields, display configured/enabled state and target count, and never fetch or render the saved proxy URL.

- [ ] **Step 3: Persist updates through CPA**

Validate form values and call:

```js
call("/v0/management/plugins/cpa-plugin-codex-turn-state/config", "PATCH", payload)
```

Only include `collector_proxy_url` when the operator entered a replacement. Clear it explicitly with `{ collector_proxy_url: null }`.

- [ ] **Step 4: Keep demo mode side-effect free**

Update local demo collector state without making a network request so the settings panel can be visually verified.

- [ ] **Step 5: Run the focused test and verify GREEN**

Run: `go test ./cmd/plugin -run TestDashboardOffersPersistentCollectorSettings -count=1`

Expected: PASS.

### Task 3: Verify behavior and layout

**Files:**
- Modify if required: `cmd/plugin/dashboard.html`
- Update if required: `.omx/state/dashboard-collector-settings/ralph-progress.json`

- [ ] **Step 1: Run repository checks**

Run:

```bash
gofmt -l cmd/plugin/*.go
git diff --check
go test ./... -count=1
go vet ./...
go test -race ./cmd/plugin -count=1
```

Expected: all commands pass with no formatting output.

- [ ] **Step 2: Open demo settings and capture a screenshot**

Open `http://127.0.0.1:8787/dashboard.html#demo`, expand `采集设置`, and verify all controls fit without covering the table actions.

- [ ] **Step 3: Record the visual verdict**

Persist a JSON verdict with score, differences, suggestions, reasoning, and next actions. Iterate if the score is below 90.

- [ ] **Step 4: Rebuild the Linux/amd64 plugin**

Run: `./scripts/build-linux-amd64.sh`

Expected: `ABI_SMOKE_OK` and an ELF64 x86-64 shared object exporting `cliproxy_plugin_init`.

Commits are intentionally omitted because the user did not request repository commits.

