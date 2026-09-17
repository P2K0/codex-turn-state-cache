package turnstate

import (
	"sync"
	"time"
)

const (
	TurnStateHeader = "X-Codex-Turn-State"
	StateLength     = 292

	stateTTL   = time.Hour
	pendingTTL = 2 * time.Hour
)

// CacheKey scopes state to the exact CPA-selected credential and model.
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

type Cache struct {
	mu                sync.Mutex
	now               func() time.Time
	maxEntries        int
	maxPendingEntries int
	entries           map[CacheKey]stateEntry
	pending           map[string]pendingBinding
}

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

// BindRequest associates a request ID with the trusted after-auth key.
func (c *Cache) BindRequest(requestID string, key CacheKey) {
	if requestID == "" {
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

func (c *Cache) ForgetRequest(requestID string) {
	if requestID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, requestID)
}

// Lookup returns a state only while its capture-time lifetime is valid.
func (c *Cache) Lookup(key CacheKey) (string, bool) {
	if !validKey(key) {
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

// StoreResponseForRequest writes a valid state for the request's after-auth key.
func (c *Cache) StoreResponseForRequest(requestID string, values []string) (key CacheKey, stored, replaced bool) {
	if requestID == "" {
		return CacheKey{}, false, false
	}
	value, ok := validStateValue(values)
	if !ok {
		return CacheKey{}, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	binding, ok := c.pending[requestID]
	if !ok {
		return CacheKey{}, false, false
	}
	return binding.key, true, c.storeLocked(binding.key, value, now)
}

func (c *Cache) storeLocked(key CacheKey, value string, now time.Time) bool {
	_, replaced := c.entries[key]
	if !replaced {
		c.evictEntriesLocked()
	}
	c.entries[key] = stateEntry{
		value:      value,
		capturedAt: now,
		expiresAt:  now.Add(stateTTL),
	}
	return replaced
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
	if len(values) != 1 || len(values[0]) != StateLength {
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
