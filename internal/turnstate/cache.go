package turnstate

import (
	"sync"
	"time"
)

const (
	// TurnStateHeader is the only response/request header managed by this plugin.
	TurnStateHeader = "X-Codex-Turn-State"
	// StateLength is the exact raw byte length accepted from an upstream response.
	StateLength = 292

	stateTTL   = time.Hour
	pendingTTL = 2 * time.Hour
)

// CacheKey scopes a state to the exact CPA-selected credential record and model.
type CacheKey struct {
	AuthID string
	Model  string
}

type stateEntry struct {
	value      string
	capturedAt time.Time
	expiresAt  time.Time
}

type pendingBinding struct {
	key       CacheKey
	createdAt time.Time
	expiresAt time.Time
}

// Cache holds process-local state and per-request correlations. It never persists data.
type Cache struct {
	mu                sync.Mutex
	now               func() time.Time
	maxEntries        int
	maxPendingEntries int
	entries           map[CacheKey]stateEntry
	pending           map[string]pendingBinding
}

// NewCache returns a bounded, process-local state cache.
func NewCache(maxEntries, maxPendingEntries int, now func() time.Time) *Cache {
	if maxEntries < 1 {
		maxEntries = 1
	}
	if maxPendingEntries < 1 {
		maxPendingEntries = 1
	}
	if now == nil {
		now = time.Now
	}
	return &Cache{
		now:               now,
		maxEntries:        maxEntries,
		maxPendingEntries: maxPendingEntries,
		entries:           make(map[CacheKey]stateEntry),
		pending:           make(map[string]pendingBinding),
	}
}

// BindRequest associates a request ID with the trusted after-auth cache key.
// A retry for the same request ID deliberately overwrites the earlier selection.
func (c *Cache) BindRequest(requestID string, key CacheKey) {
	if c == nil || requestID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	if !validKey(key) {
		delete(c.pending, requestID)
		return
	}
	if _, exists := c.pending[requestID]; !exists {
		c.evictPendingLocked()
	}
	c.pending[requestID] = pendingBinding{
		key:       key,
		createdAt: now,
		expiresAt: now.Add(pendingTTL),
	}
}

// ForgetRequest drops request-local state after completion or an incomplete hook context.
func (c *Cache) ForgetRequest(requestID string) {
	if c == nil || requestID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, requestID)
}

// Pending returns the trusted after-auth association for one outstanding request.
func (c *Cache) Pending(requestID string) (CacheKey, bool) {
	if c == nil || requestID == "" {
		return CacheKey{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	binding, ok := c.pending[requestID]
	if !ok {
		return CacheKey{}, false
	}
	return binding.key, true
}

// Lookup returns a state only while its fixed capture-time lifetime is valid.
func (c *Cache) Lookup(key CacheKey) (string, bool) {
	if c == nil || !validKey(key) {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	return entry.value, true
}

// StoreResponse validates and stores a response value for an already-known cache key.
func (c *Cache) StoreResponse(key CacheKey, values []string) bool {
	if c == nil || !validKey(key) {
		return false
	}
	value, ok := validStateValue(values)
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	c.storeLocked(key, value, now)
	return true
}

// StoreResponseForRequest stores against the trusted key bound during after-auth.
func (c *Cache) StoreResponseForRequest(requestID string, values []string) bool {
	if c == nil || requestID == "" {
		return false
	}
	value, ok := validStateValue(values)
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	binding, ok := c.pending[requestID]
	if !ok {
		return false
	}
	c.storeLocked(binding.key, value, now)
	return true
}

func (c *Cache) storeLocked(key CacheKey, value string, now time.Time) {
	if _, exists := c.entries[key]; !exists {
		c.evictEntriesLocked()
	}
	c.entries[key] = stateEntry{
		value:      value,
		capturedAt: now,
		expiresAt:  now.Add(stateTTL),
	}
}

func (c *Cache) purgeExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	for requestID, binding := range c.pending {
		if !now.Before(binding.expiresAt) {
			delete(c.pending, requestID)
		}
	}
}

func (c *Cache) evictEntriesLocked() {
	for len(c.entries) >= c.maxEntries {
		var oldestKey CacheKey
		var oldest stateEntry
		first := true
		for key, entry := range c.entries {
			if first || entry.capturedAt.Before(oldest.capturedAt) || (entry.capturedAt.Equal(oldest.capturedAt) && cacheKeyLess(key, oldestKey)) {
				oldestKey = key
				oldest = entry
				first = false
			}
		}
		delete(c.entries, oldestKey)
	}
}

func (c *Cache) evictPendingLocked() {
	for len(c.pending) >= c.maxPendingEntries {
		var oldestRequestID string
		var oldest pendingBinding
		first := true
		for requestID, binding := range c.pending {
			if first || binding.createdAt.Before(oldest.createdAt) || (binding.createdAt.Equal(oldest.createdAt) && requestID < oldestRequestID) {
				oldestRequestID = requestID
				oldest = binding
				first = false
			}
		}
		delete(c.pending, oldestRequestID)
	}
}

func validKey(key CacheKey) bool {
	return key.AuthID != "" && key.Model != ""
}

func validStateValue(values []string) (string, bool) {
	if len(values) != 1 || values[0] == "" || len([]byte(values[0])) != StateLength {
		return "", false
	}
	return values[0], true
}

func cacheKeyLess(left, right CacheKey) bool {
	if left.AuthID != right.AuthID {
		return left.AuthID < right.AuthID
	}
	return left.Model < right.Model
}
