package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gstern-CTO/huginn/internal/metrics"
)

// Cache is a two-tier store: an in-memory tier for the current session and a
// disk tier that survives restarts.
//
// The disk tier is the point. An MCP server is restarted on every client
// session, and without persistence the same repository trees and the same
// frequently-read files are re-fetched from GitHub every time
// (WEAKNESSES.md #7).
type Cache struct {
	mu      sync.RWMutex
	mem     map[string]memEntry
	maxMem  int
	dir     string
	ttl     time.Duration
	metrics *metrics.Metrics

	// now is injectable so expiry is testable without sleeping.
	now func() time.Time
}

type memEntry struct {
	value     []byte
	expiresAt time.Time
}

// diskEntry is the on-disk representation. Key is stored alongside the value so
// a cache file is self-describing when debugging.
type diskEntry struct {
	Key       string    `json:"key"`
	ExpiresAt time.Time `json:"expiresAt"`
	Value     []byte    `json:"value"`
}

const defaultMaxMemEntries = 2048

// NewCache builds a cache rooted at dir. A disk failure is never fatal: the
// cache degrades to memory-only rather than taking the server down.
func NewCache(dir string, ttl time.Duration, metrics *metrics.Metrics) *Cache {
	c := &Cache{
		mem:     make(map[string]memEntry),
		maxMem:  defaultMaxMemEntries,
		dir:     dir,
		ttl:     ttl,
		metrics: metrics,
		now:     time.Now,
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			c.dir = "" // memory-only fallback
		}
	}
	return c
}

// CacheKey builds a stable key from a namespace and ordered parts. The same
// inputs always produce the same key: parts are length-prefixed so that
// ("ab","c") and ("a","bc") cannot collide.
func CacheKey(namespace string, parts ...string) string {
	var sb strings.Builder
	sb.WriteString(namespace)
	for _, p := range parts {
		sb.WriteByte('\x1f')
		sb.WriteString(p)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return namespace + "-" + hex.EncodeToString(sum[:16])
}

// CacheKeyMap builds a stable key from an unordered map by sorting its keys, so
// that argument order in the caller cannot change the resulting cache key.
func CacheKeyMap(namespace string, m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		parts = append(parts, k, m[k])
	}
	return CacheKey(namespace, parts...)
}

// Get returns the cached bytes for key. A hit in the disk tier is promoted into
// memory so a second read in the same session is served without disk I/O.
func (c *Cache) Get(key string) ([]byte, bool) {
	now := c.now()

	c.mu.RLock()
	entry, ok := c.mem[key]
	c.mu.RUnlock()
	if ok {
		if now.Before(entry.expiresAt) {
			c.metrics.RecordCacheHit()
			return entry.value, true
		}
		c.mu.Lock()
		delete(c.mem, key)
		c.mu.Unlock()
	}

	value, ok := c.readDisk(key, now)
	if !ok {
		c.metrics.RecordCacheMiss()
		return nil, false
	}
	c.storeMem(key, value, now.Add(c.ttl))
	c.metrics.RecordCacheHit()
	return value, true
}

// Set writes to both tiers. A zero ttl uses the cache default.
func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	if ttl <= 0 {
		ttl = c.ttl
	}
	expires := c.now().Add(ttl)
	c.storeMem(key, value, expires)
	c.writeDisk(key, value, expires)
}

// GetJSON decodes a cached JSON value into out.
func (c *Cache) GetJSON(key string, out any) bool {
	raw, ok := c.Get(key)
	if !ok {
		return false
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false
	}
	return true
}

// SetJSON encodes value as JSON and caches it. Encoding failures are silently
// ignored: a cache write is never worth failing a request over.
func (c *Cache) SetJSON(key string, value any, ttl time.Duration) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	c.Set(key, raw, ttl)
}

func (c *Cache) storeMem(key string, value []byte, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.mem) >= c.maxMem {
		c.evictLocked()
	}
	c.mem[key] = memEntry{value: value, expiresAt: expires}
}

// evictLocked drops expired entries, and if that frees nothing, drops an
// arbitrary slice of the map. The cache is an optimisation, so approximate
// eviction is acceptable and avoids maintaining an LRU list on the hot path.
func (c *Cache) evictLocked() {
	now := c.now()
	for k, v := range c.mem {
		if now.After(v.expiresAt) {
			delete(c.mem, k)
		}
	}
	if len(c.mem) < c.maxMem {
		return
	}
	target := len(c.mem) / 4
	for k := range c.mem {
		if target <= 0 {
			break
		}
		delete(c.mem, k)
		target--
	}
}

func (c *Cache) diskPath(key string) string {
	if c.dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:])
	// Shard by the first byte to keep directory sizes reasonable.
	return filepath.Join(c.dir, name[:2], name+".json")
}

func (c *Cache) readDisk(key string, now time.Time) ([]byte, bool) {
	path := c.diskPath(key)
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entry diskEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		_ = os.Remove(path) // corrupt entry: drop it
		return nil, false
	}
	if entry.Key != key || now.After(entry.ExpiresAt) {
		_ = os.Remove(path)
		return nil, false
	}
	return entry.Value, true
}

func (c *Cache) writeDisk(key string, value []byte, expires time.Time) {
	path := c.diskPath(key)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(diskEntry{Key: key, ExpiresAt: expires, Value: value})
	if err != nil {
		return
	}
	// Write to a temporary file and rename, so a crash mid-write cannot leave
	// a truncated entry that later reads would have to defend against.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}

// Purge removes every entry from both tiers.
func (c *Cache) Purge() error {
	c.mu.Lock()
	c.mem = make(map[string]memEntry)
	c.mu.Unlock()
	if c.dir == "" {
		return nil
	}
	return os.RemoveAll(c.dir)
}
