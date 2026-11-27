package namespace

import (
	"container/list"
	"context"
	"errors"
	"slices"
	"sync"
)

// DefaultCacheEntries bounds the number of distinct lookup snapshots, not the
// number of rows in a snapshot. Complete result sets are never truncated.
const DefaultCacheEntries = 128

type finder interface {
	Find(context.Context, string, string) ([]Namespace, error)
}

type cacheKey struct {
	mode, target string
}

type cacheEntry struct {
	key   cacheKey
	items []Namespace
}

// Cache is a process-local, least-recently-used session snapshot. There is no
// TTL or stale-on-error fallback. Create one instance per database/store; never
// reuse an instance after switching database configuration.
//
// Cache must not be copied. Hits can proceed while a query runs. Misses are
// serialized through a context-aware gate, avoiding duplicate queries without
// retaining caller contexts, errors, or an unbounded set of in-flight keys.
type Cache struct {
	source finder
	limit  int
	gate   chan struct{}

	mu         sync.Mutex
	entries    map[cacheKey]*list.Element
	recent     list.List
	generation uint64
}

// NewCache wraps one fixed database discovery source. maxEntries must be positive.
func NewCache(source finder, maxEntries int) (*Cache, error) {
	if source == nil || maxEntries <= 0 {
		return nil, errors.New("namespace cache requires a source and a positive entry limit")
	}
	c := &Cache{source: source, limit: maxEntries, gate: make(chan struct{}, 1), entries: make(map[cacheKey]*list.Element)}
	c.gate <- struct{}{}
	return c, nil
}

func (c *Cache) Find(ctx context.Context, mode, target string) ([]Namespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, _, err := Query(mode, target); err != nil {
		return nil, err
	}
	key := cacheKey{mode: mode, target: target}
	c.mu.Lock()
	items, hit := c.lookup(key)
	c.mu.Unlock()
	if hit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return items, nil
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Another caller may have filled the entry while this caller waited.
	c.mu.Lock()
	items, hit = c.lookup(key)
	generation := c.generation
	c.mu.Unlock()
	if hit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return items, nil
	}
	items, err := c.source.Find(ctx, mode, target)
	if err != nil {
		// Drop even nonempty partial results; errors are never cache entries.
		return nil, err
	}
	snapshot := slices.Clone(items)
	for _, n := range snapshot {
		if err := n.Validate(); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if generation == c.generation {
		c.entries[key] = c.recent.PushFront(cacheEntry{key: key, items: snapshot})
		if len(c.entries) > c.limit {
			oldest := c.recent.Back()
			delete(c.entries, oldest.Value.(cacheEntry).key)
			c.recent.Remove(oldest)
		}
	}
	return slices.Clone(snapshot), nil
}

// lookup requires c.mu. All cache-owned slices remain private.
func (c *Cache) lookup(key cacheKey) ([]Namespace, bool) {
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.recent.MoveToFront(element)
	return slices.Clone(element.Value.(cacheEntry).items), true
}

// Clear invalidates all entries. A query already in progress may return its
// result to its original caller, but cannot repopulate the cleared cache.
// Subsequent lookups query afresh. Restarting the CLI also discards the cache.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.entries = make(map[cacheKey]*list.Element)
	c.recent.Init()
}
