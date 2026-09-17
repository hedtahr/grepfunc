package grepfunc

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// cacheStabilityWindow keeps recently written files out of the cache: a file an
// agent just edited must be re-read every time, whatever the clock resolution
// says. Cheap protection for the edit-then-search loop.
const cacheStabilityWindow = 2 * time.Second

// Cache bounds: room for a medium repo, evicted oldest-first, with huge files
// excluded so one of them cannot flush everything.
const (
	cacheMaxBytes = 64 << 20
	cacheMaxEntry = 4 << 20
)

type cacheEntry struct {
	path string
	size int64
	mod  int64 // UnixNano
	data []byte
}

// fileCache holds file contents keyed by path, size and modification time. Keys
// come from the stat the walker already did, so a hit costs no syscall at all.
type fileCache struct {
	mu       sync.Mutex
	entries  map[string]*cacheEntry
	order    []*cacheEntry
	head     int
	bytes    int
	maxBytes int
	hits     atomic.Int64
	misses   atomic.Int64
}

//nolint:gochecknoglobals // one process-wide read cache
var fileReadCache = newFileCache(cacheMaxBytes)

func newFileCache(maxBytes int) *fileCache {
	return &fileCache{entries: make(map[string]*cacheEntry), maxBytes: maxBytes}
}

// ReadCachedFile reads a file, reusing the contents of an unchanged path. The
// returned slice is shared with the cache: callers must not modify it. size and
// mod must come from a stat of path.
func ReadCachedFile(path string, size int64, mod time.Time) ([]byte, error) {
	return fileReadCache.read(path, size, mod)
}

func (c *fileCache) read(path string, size int64, mod time.Time) ([]byte, error) {
	if data, ok := c.lookup(path, size, mod.UnixNano()); ok {
		return data, nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- callers bounds-check paths
	if err != nil {
		return nil, err
	}

	c.misses.Add(1)
	c.store(path, size, mod, data)

	return data, nil
}

func (c *fileCache) lookup(path string, size, modNano int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[path]
	if !ok || entry.size != size || entry.mod != modNano {
		return nil, false
	}

	c.hits.Add(1)

	return entry.data, true
}

func (c *fileCache) store(path string, size int64, mod time.Time, data []byte) {
	if size > cacheMaxEntry || time.Since(mod) < cacheStabilityWindow {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[path]; exists {
		return
	}

	entry := &cacheEntry{path: path, size: size, mod: mod.UnixNano(), data: data}

	c.entries[path] = entry
	c.order = append(c.order, entry)
	c.bytes += len(data)

	c.evict()
}

// evict drops oldest-first entries until the byte budget fits again.
func (c *fileCache) evict() {
	for c.bytes > c.maxBytes && c.head < len(c.order) {
		victim := c.order[c.head]
		c.head++

		if c.entries[victim.path] == victim {
			delete(c.entries, victim.path)

			c.bytes -= len(victim.data)
		}
	}

	// Compact the queue once most of it has been consumed.
	if c.head > 1024 && c.head*2 > len(c.order) {
		c.order = append(c.order[:0], c.order[c.head:]...)
		c.head = 0
	}
}

// cachedBytes reports retained bytes, for tests and diagnostics.
func (c *fileCache) cachedBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.bytes
}
