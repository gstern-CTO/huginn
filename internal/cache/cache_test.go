package cache

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gstern-CTO/huginn/internal/metrics"
)

func TestCacheHitAndMiss(t *testing.T) {
	metrics := metrics.NewMetrics(false, 0)
	cache := NewCache(t.TempDir(), time.Hour, metrics)

	_, ok := cache.Get("absent")
	require.False(t, ok, "an unknown key must miss")

	cache.Set("present", []byte("value"), 0)

	got, ok := cache.Get("present")
	require.True(t, ok)
	require.Equal(t, []byte("value"), got)

	require.Equal(t, int64(1), metrics.Hits())
	require.Equal(t, int64(1), metrics.Misses())
}

func TestCacheExpiry(t *testing.T) {
	cache := NewCache(t.TempDir(), time.Hour, metrics.NewMetrics(false, 0))

	now := time.Now()
	cache.now = func() time.Time { return now }
	cache.Set("k", []byte("v"), time.Minute)

	_, ok := cache.Get("k")
	require.True(t, ok, "the entry is live before its TTL")

	// Advance past the TTL without sleeping.
	cache.now = func() time.Time { return now.Add(2 * time.Minute) }
	_, ok = cache.Get("k")
	require.False(t, ok, "the entry must expire once its TTL passes")
}

// The disk tier is the reason this cache exists: an MCP server restarts on
// every client session, and an in-memory-only cache starts cold every time.
func TestCachePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	first := NewCache(dir, time.Hour, metrics.NewMetrics(false, 0))
	first.Set("repo-tree", []byte(`{"entries":["a","b"]}`), 0)

	// A brand-new instance stands in for a restarted process.
	second := NewCache(dir, time.Hour, metrics.NewMetrics(false, 0))
	got, ok := second.Get("repo-tree")
	require.True(t, ok, "the disk tier must survive a restart")
	require.Equal(t, `{"entries":["a","b"]}`, string(got))
}

func TestCacheDiskEntryExpiresAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	first := NewCache(dir, time.Hour, metrics.NewMetrics(false, 0))
	first.now = func() time.Time { return now }
	first.Set("k", []byte("v"), time.Minute)

	second := NewCache(dir, time.Hour, metrics.NewMetrics(false, 0))
	second.now = func() time.Time { return now.Add(2 * time.Minute) }
	_, ok := second.Get("k")
	require.False(t, ok, "an expired disk entry must not be served")
}

func TestCacheDiskHitPromotesToMemory(t *testing.T) {
	dir := t.TempDir()

	first := NewCache(dir, time.Hour, metrics.NewMetrics(false, 0))
	first.Set("k", []byte("v"), 0)

	second := NewCache(dir, time.Hour, metrics.NewMetrics(false, 0))
	_, ok := second.Get("k")
	require.True(t, ok)

	second.mu.RLock()
	_, inMemory := second.mem["k"]
	second.mu.RUnlock()
	require.True(t, inMemory, "a disk hit must be promoted so the next read skips disk I/O")
}

func TestCacheJSONRoundTrip(t *testing.T) {
	cache := NewCache(t.TempDir(), time.Hour, metrics.NewMetrics(false, 0))

	type payload struct {
		Path  string `json:"path"`
		Lines int    `json:"lines"`
	}
	cache.SetJSON("file", payload{Path: "main.go", Lines: 42}, 0)

	var got payload
	require.True(t, cache.GetJSON("file", &got))
	require.Equal(t, payload{Path: "main.go", Lines: 42}, got)
}

// Cache keys must be stable across calls and must not collide when the same
// characters are split differently between parts.
func TestCacheKeyStabilityAndCollisionResistance(t *testing.T) {
	require.Equal(t,
		CacheKey("ghfile", "owner", "repo", "", "main.go"),
		CacheKey("ghfile", "owner", "repo", "", "main.go"),
		"the same inputs must always produce the same key")

	require.NotEqual(t,
		CacheKey("ghfile", "ab", "c"),
		CacheKey("ghfile", "a", "bc"),
		"differently split parts must not collide")

	require.NotEqual(t,
		CacheKey("ghfile", "owner", "repo", "", "main.go"),
		CacheKey("ghtree", "owner", "repo", "", "main.go"),
		"different namespaces must not collide")
}

// Argument order in the caller must not change the key.
func TestCacheKeyMapIsOrderIndependent(t *testing.T) {
	a := CacheKeyMap("search", map[string]string{"owner": "acme", "repo": "widget", "ref": "main"})
	b := CacheKeyMap("search", map[string]string{"ref": "main", "repo": "widget", "owner": "acme"})
	require.Equal(t, a, b)
}

func TestCacheFallsBackToMemoryWhenDiskUnavailable(t *testing.T) {
	// A path under a regular file cannot be created as a directory.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, writeFileForTest(blocker, "not a directory"))

	cache := NewCache(filepath.Join(blocker, "cache"), time.Hour, metrics.NewMetrics(false, 0))
	require.Empty(t, cache.dir, "an unusable cache directory must degrade to memory-only")

	cache.Set("k", []byte("v"), 0)
	got, ok := cache.Get("k")
	require.True(t, ok, "the memory tier must still work")
	require.Equal(t, []byte("v"), got)
}

func TestCacheEvictsWhenFull(t *testing.T) {
	cache := NewCache("", time.Hour, metrics.NewMetrics(false, 0))
	cache.maxMem = 8

	for i := 0; i < 40; i++ {
		cache.Set(CacheKey("k", strconv.Itoa(i)), []byte("v"), 0)
	}

	cache.mu.RLock()
	size := len(cache.mem)
	cache.mu.RUnlock()
	require.LessOrEqual(t, size, cache.maxMem, "the memory tier must stay bounded")
}

func TestCachePurge(t *testing.T) {
	dir := t.TempDir()
	cache := NewCache(dir, time.Hour, metrics.NewMetrics(false, 0))
	cache.Set("k", []byte("v"), 0)

	require.NoError(t, cache.Purge())

	_, ok := cache.Get("k")
	require.False(t, ok)
}
