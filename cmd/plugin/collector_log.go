package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/5345asda/codex-turn-state-cache/internal/turnstate"
)

const (
	defaultCollectorEventLogPath = "data/codex-turn-state-cache/collector-events.jsonl"
	collectorEventTailLimit      = 50
	collectorActionDiscovered    = "target_discovered"
	collectorActionStarted       = "collection_started"
	collectorActionSucceeded     = "collection_succeeded"
	collectorActionFailed        = "collection_failed"
	collectorActionConfigured    = "collector_configured"
)

type collectorEvent struct {
	At             string `json:"at"`
	Level          string `json:"level"`
	Action         string `json:"action"`
	Source         string `json:"source,omitempty"`
	AuthID         string `json:"auth_id,omitempty"`
	Model          string `json:"model,omitempty"`
	StatusCode     int    `json:"status_code,omitempty"`
	StateLength    int    `json:"state_length,omitempty"`
	RetryInSeconds int    `json:"retry_in_seconds,omitempty"`
	Message        string `json:"message"`
}

type collectorLogStatus struct {
	Path             string `json:"path"`
	AppendOnly       bool   `json:"append_only"`
	NextBefore       int64  `json:"next_before,omitempty"`
	MalformedRecords int    `json:"malformed_records,omitempty"`
	Error            string `json:"error,omitempty"`
}

type collectorEventPage struct {
	Events     []collectorEvent
	NextBefore int64
}

type collectorEventStore struct {
	mu               sync.Mutex
	path             string
	lastErr          string
	malformedRecords int
}

func newCollectorEventStore(path string) *collectorEventStore {
	return &collectorEventStore{path: filepath.Clean(strings.TrimSpace(path))}
}

func accountAlias(authID string) string {
	digest := sha256.Sum256([]byte(authID))
	return fmt.Sprintf("%x", digest[:6])
}

func sanitizeCollectorEvent(event collectorEvent) collectorEvent {
	if event.AuthID != "" {
		event.AuthID = accountAlias(event.AuthID)
	}
	lowerMessage := strings.ToLower(event.Message)
	for _, marker := range []string{"token", "proxy", "state", "error", "password", "bearer"} {
		if strings.Contains(lowerMessage, marker) {
			event.Message = "collector event"
			break
		}
	}
	return event
}

func refuseCollectorLogSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("collector event log refuses symlink target")
	}
	return nil
}

func (store *collectorEventStore) Append(event collectorEvent) error {
	if store == nil || store.path == "." || store.path == "" {
		return fmt.Errorf("collector event log path is empty")
	}
	if event.At == "" {
		event.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if event.Level == "" {
		event.Level = "info"
	}
	event = sanitizeCollectorEvent(event)
	raw, errMarshal := json.Marshal(event)
	if errMarshal != nil {
		return errMarshal
	}
	raw = append(raw, '\n')

	store.mu.Lock()
	defer store.mu.Unlock()
	if errMkdir := os.MkdirAll(filepath.Dir(store.path), 0o700); errMkdir != nil {
		store.lastErr = errMkdir.Error()
		return errMkdir
	}
	if errChmod := os.Chmod(filepath.Dir(store.path), 0o700); errChmod != nil {
		store.lastErr = errChmod.Error()
		return errChmod
	}
	if errSymlink := refuseCollectorLogSymlink(store.path); errSymlink != nil {
		store.lastErr = errSymlink.Error()
		return errSymlink
	}
	file, errOpen := os.OpenFile(store.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if errOpen != nil {
		store.lastErr = errOpen.Error()
		return errOpen
	}
	if errChmod := file.Chmod(0o600); errChmod != nil {
		_ = file.Close()
		store.lastErr = errChmod.Error()
		return errChmod
	}
	if errRepair := repairCollectorLogTail(file); errRepair != nil {
		_ = file.Close()
		store.lastErr = errRepair.Error()
		return errRepair
	}
	_, errWrite := file.Write(raw)
	if errWrite == nil {
		errWrite = file.Sync()
	}
	errClose := file.Close()
	if errWrite == nil {
		errWrite = errClose
	}
	if errWrite != nil {
		store.lastErr = errWrite.Error()
		return errWrite
	}
	store.lastErr = ""
	return nil
}

func (store *collectorEventStore) Tail(limit int) ([]collectorEvent, error) {
	page, errPage := store.Page(0, limit)
	return page.Events, errPage
}

func (store *collectorEventStore) Page(before int64, limit int) (collectorEventPage, error) {
	if store == nil || limit < 1 {
		return collectorEventPage{Events: []collectorEvent{}}, nil
	}
	if limit > 500 {
		limit = 500
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	lines, nextBefore, errRead := tailFilePage(store.path, before, limit)
	if errors.Is(errRead, os.ErrNotExist) {
		return collectorEventPage{Events: []collectorEvent{}}, nil
	}
	if errRead != nil {
		store.lastErr = errRead.Error()
		return collectorEventPage{}, errRead
	}
	store.malformedRecords, errRead = countMalformedCollectorRecords(store.path)
	if errRead != nil {
		store.lastErr = errRead.Error()
		return collectorEventPage{}, errRead
	}
	events := make([]collectorEvent, 0, len(lines))
	for index := len(lines) - 1; index >= 0; index-- {
		var event collectorEvent
		if errUnmarshal := json.Unmarshal(lines[index], &event); errUnmarshal == nil {
			events = append(events, event)
		}
	}
	return collectorEventPage{Events: events, NextBefore: nextBefore}, nil
}

func (store *collectorEventStore) Status() collectorLogStatus {
	if store == nil {
		return collectorLogStatus{}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return collectorLogStatus{Path: store.path, AppendOnly: true, MalformedRecords: store.malformedRecords, Error: store.lastErr}
}

func repairCollectorLogTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	last := []byte{0}
	if _, err = file.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	position := info.Size()
	const blockSize int64 = 64 * 1024
	for position > 0 {
		readSize := blockSize
		if position < readSize {
			readSize = position
		}
		position -= readSize
		chunk := make([]byte, readSize)
		if _, err := file.ReadAt(chunk, position); err != nil {
			return err
		}
		if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
			return file.Truncate(position + int64(index) + 1)
		}
	}
	return file.Truncate(0)
}

func countMalformedCollectorRecords(path string) (int, error) {
	if err := refuseCollectorLogSymlink(path); err != nil {
		return 0, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		return 0, err
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		if index := bytes.LastIndexByte(raw, '\n'); index >= 0 {
			raw = raw[:index+1]
		} else {
			raw = nil
		}
	}
	malformed := 0
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event collectorEvent
		if err := json.Unmarshal(line, &event); err != nil {
			malformed++
		}
	}
	return malformed, nil
}

func tailFileLines(path string, limit int) ([][]byte, error) {
	lines, _, errPage := tailFilePage(path, 0, limit)
	return lines, errPage
}

func tailFilePage(path string, before int64, limit int) ([][]byte, int64, error) {
	if err := refuseCollectorLogSymlink(path); err != nil {
		return nil, 0, err
	}
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return nil, 0, errOpen
	}
	defer file.Close()
	info, errStat := file.Stat()
	if errStat != nil {
		return nil, 0, errStat
	}
	const blockSize int64 = 64 * 1024
	end := before
	if end <= 0 || end > info.Size() {
		end = info.Size()
		if end > 0 {
			last := []byte{0}
			if _, errRead := file.ReadAt(last, end-1); errRead != nil {
				return nil, 0, errRead
			}
			if last[0] != '\n' {
				position := end
				for position > 0 {
					readSize := blockSize
					if position < readSize {
						readSize = position
					}
					position -= readSize
					chunk := make([]byte, readSize)
					if _, errRead := file.ReadAt(chunk, position); errRead != nil {
						return nil, 0, errRead
					}
					if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
						end = position + int64(index) + 1
						break
					}
				}
			}
		}
	}
	position := end
	buffer := []byte{}
	for position > 0 {
		readSize := blockSize
		if position < readSize {
			readSize = position
		}
		position -= readSize
		chunk := make([]byte, readSize)
		if _, errRead := file.ReadAt(chunk, position); errRead != nil {
			return nil, 0, errRead
		}
		buffer = append(chunk, buffer...)
		trimmed := bytes.TrimSuffix(buffer, []byte{'\n'})
		lineCount := bytes.Count(trimmed, []byte{'\n'}) + 1
		if lineCount >= limit && (position == 0 || lineCount > limit) {
			break
		}
	}
	buffer = bytes.TrimSuffix(buffer, []byte{'\n'})
	if len(buffer) == 0 {
		return [][]byte{}, 0, nil
	}
	lines := bytes.Split(buffer, []byte{'\n'})
	start := 0
	if len(lines) > limit {
		start = len(lines) - limit
	}
	nextBefore := int64(0)
	if start > 0 || position > 0 {
		nextBefore = position
		for index := 0; index < start; index++ {
			nextBefore += int64(len(lines[index]) + 1)
		}
	}
	return lines[start:], nextBefore, nil
}

func recordCollectorEvent(ctx context.Context, event collectorEvent) {
	if event.At == "" {
		event.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if event.Level == "" {
		event.Level = "info"
	}
	store := currentCollectorEventStore()
	if hostAPIAvailable() && store != nil {
		if errAppend := store.Append(event); errAppend != nil {
			event.Level = "error"
			event.Message = "采集日志写入失败"
		}
	}
	logCollectorEvent(ctx, sanitizeCollectorEvent(event))
}

func collectorResultEvent(result collectorProbeResult, source string, key turnstate.CacheKey, retryAfter time.Duration) collectorEvent {
	event := collectorEvent{
		Level:       "warn",
		Action:      collectorActionFailed,
		Source:      source,
		AuthID:      key.AuthID,
		Model:       key.Model,
		StatusCode:  result.StatusCode,
		StateLength: result.Length,
	}
	if retryAfter > 0 {
		event.RetryInSeconds = int(retryAfter.Seconds())
	}
	if result.Harvested {
		event.Level = "info"
		event.Action = collectorActionSucceeded
		event.RetryInSeconds = 0
		event.Message = "已采集并缓存新鲜 292 模板"
		return event
	}
	switch {
	case result.Err != nil:
		event.Message = "采集请求失败"
	case result.StatusCode >= 400:
		event.Message = "上游返回非成功状态"
	case result.Length == 0:
		event.Message = "上游未返回 turn-state"
	case result.Length == 312:
		event.Message = "上游返回降级 312 状态"
	default:
		event.Message = "采集结果未通过校验"
	}
	return event
}
