package namespace

import (
	"container/list"
	"context"
	"eks/internal/diagnostics"
	"errors"
	"log/slog"
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
	// Diagnostics is used only for context-free explicit invalidation.
	Diagnostics *diagnostics.Diagnostics
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

func (c *Cache) Find(ctx context.Context, mode, target string) (result []Namespace, err error) {
	ctx, scope := diagnostics.Start(ctx, "cache", "cache_lookup")
	defer func() { err = scope.End(err) }()
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
		scope.Event("cache.hit", slog.LevelDebug)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return items, nil
	}
	scope.Event("cache.miss", slog.LevelDebug)
	_, waiting := diagnostics.Start(ctx, "cache", "cache_wait")
	waiting.Event("cache.wait", slog.LevelDebug)

	select {
	case <-ctx.Done():
		return nil, waiting.End(ctx.Err())
	case <-c.gate:
	}
	_ = waiting.End(nil)
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
		scope.Event("cache.hit", slog.LevelDebug)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return items, nil
	}
	items, err = c.source.Find(ctx, mode, target)
	if err != nil {
		// Drop even nonempty partial results; errors are never cache entries.
		return nil, err
	}
	snapshot := slices.Clone(items)
	for _, n := range snapshot {
		if err := n.Validate(); err != nil {
			_, validation := diagnostics.Start(ctx, "cache", "cache_validation")
			return nil, validation.End(err)
		}
	}
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	suppressed, evicted := generation != c.generation, false
	if generation == c.generation {
		c.entries[key] = c.recent.PushFront(cacheEntry{key: key, items: snapshot})
		if len(c.entries) > c.limit {
			evicted = true
			oldest := c.recent.Back()
			delete(c.entries, oldest.Value.(cacheEntry).key)
			c.recent.Remove(oldest)
		}
	}
	c.mu.Unlock()
	if suppressed {
		scope.Event("cache.suppressed", slog.LevelDebug)
	} else {
		scope.Event("cache.fill", slog.LevelDebug, diagnostics.N("row_count", len(snapshot)))
		if len(snapshot) == 0 {
			scope.Event("cache.empty", slog.LevelDebug, diagnostics.N("row_count", 0))
		}
	}
	if evicted {
		scope.Event("cache.eviction", slog.LevelDebug)
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
	c.generation++
	c.entries = make(map[cacheKey]*list.Element)
	c.recent.Init()
	c.mu.Unlock()
	ctx := diagnostics.With(context.Background(), c.Diagnostics)
	_, scope := diagnostics.Start(ctx, "cache", "cache_clear")
	scope.Event("cache.clear", slog.LevelDebug)
	_ = scope.End(nil)
}
