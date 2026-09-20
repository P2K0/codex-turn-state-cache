//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectorEventStorePersistsPrivateAliasAndPermissions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "collector-events.jsonl")
	store := newCollectorEventStore(path)
	if err := store.Append(collectorEvent{
		AuthID:  "raw-auth-secret",
		Action:  collectorActionFailed,
		Message: "proxy=https://user:password@example.invalid token=access-token state=292 error=secret-value",
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat event log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("event log mode = %04o, want 0600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat event directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("event directory mode = %04o, want 0700", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	text := string(raw)
	for _, secret := range []string{"raw-auth-secret", "access-token", "example.invalid", "secret-value"} {
		if strings.Contains(text, secret) {
			t.Fatalf("persisted log contains %q: %s", secret, text)
		}
	}
	var persisted collectorEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &persisted); err != nil {
		t.Fatalf("decode persisted event: %v", err)
	}
	if persisted.AuthID != "fa15ad526555" {
		t.Fatalf("persisted auth alias = %q", persisted.AuthID)
	}
}

func TestCollectorEventStoreRefusesSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("create target: %v", err)
	}
	path := filepath.Join(root, "collector-events.jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if err := newCollectorEventStore(path).Append(collectorEvent{Action: "should-fail"}); err == nil {
		t.Fatal("append through symlink unexpectedly succeeded")
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read symlink target: %v", err)
	}
	if len(contents) != 0 {
		t.Fatalf("symlink target was modified: %q", contents)
	}
}

func TestCollectorEventStoreReportsMalformedRecordsAndSeparatesTrailingPartial(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "collector-events.jsonl")
	initial := "{\"action\":\"old\",\"message\":\"old\"}\n{not-json}\n{\"action\":\"partial\""
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial log: %v", err)
	}
	store := newCollectorEventStore(path)
	if err := store.Append(collectorEvent{Action: "new", Message: "new"}); err != nil {
		t.Fatalf("append after partial record: %v", err)
	}
	page, err := store.Page(0, 10)
	if err != nil {
		t.Fatalf("read event page: %v", err)
	}
	if len(page.Events) != 2 || page.Events[0].Action != "new" || page.Events[1].Action != "old" {
		t.Fatalf("events after partial record = %#v", page.Events)
	}
	statusRaw, err := json.Marshal(store.Status())
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var status map[string]any
	if err := json.Unmarshal(statusRaw, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if got, ok := status["malformed_records"].(float64); !ok || got != 1 {
		t.Fatalf("malformed_records = %#v, want 1", status["malformed_records"])
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repaired log: %v", err)
	}
	if strings.Contains(string(raw), `{"action":"partial"}{`) {
		t.Fatalf("partial record was joined to appended record: %q", raw)
	}
}

func TestCollectorEventStorePaginatesRecordsAcrossReaderBlocks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "collector-events.jsonl")
	store := newCollectorEventStore(path)
	for index := 0; index < 400; index++ {
		if err := store.Append(collectorEvent{Action: "record", Message: strings.Repeat("x", 280)}); err != nil {
			t.Fatalf("append record %d: %v", index, err)
		}
	}
	page, err := store.Page(0, 17)
	if err != nil {
		t.Fatalf("read newest page: %v", err)
	}
	if len(page.Events) != 17 || page.NextBefore <= 64*1024 {
		t.Fatalf("newest page len=%d next_before=%d", len(page.Events), page.NextBefore)
	}
	older, err := store.Page(page.NextBefore, 17)
	if err != nil {
		t.Fatalf("read older page: %v", err)
	}
	if len(older.Events) != 17 || older.NextBefore >= page.NextBefore {
		t.Fatalf("older page len=%d next_before=%d prior=%d", len(older.Events), older.NextBefore, page.NextBefore)
	}
}
