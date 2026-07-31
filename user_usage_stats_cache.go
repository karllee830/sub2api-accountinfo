package main

import (
	"context"
	"sync"
	"time"
)

const (
	userUsageStatsCacheTTL      = 30 * time.Second
	userUsageStatsErrorCacheTTL = 5 * time.Second
)

type userUsageStatsCacheKey struct {
	userID      int64
	accountID   int64
	windowKey   string
	windowStart int64
	provisional bool
}

type userUsageStatsCacheEntry struct {
	stats     userWindowStats
	err       *upstreamAPIError
	expiresAt time.Time
}

type userUsageStatsFlight struct {
	done  chan struct{}
	stats userWindowStats
	err   *upstreamAPIError
	force bool
}

type userUsageStatsCache struct {
	mutex    sync.Mutex
	entries  map[userUsageStatsCacheKey]userUsageStatsCacheEntry
	inflight map[userUsageStatsCacheKey]*userUsageStatsFlight
	now      func() time.Time
}

func newUserUsageStatsCache() *userUsageStatsCache {
	return &userUsageStatsCache{
		entries:  make(map[userUsageStatsCacheKey]userUsageStatsCacheEntry),
		inflight: make(map[userUsageStatsCacheKey]*userUsageStatsFlight),
		now:      time.Now,
	}
}

func (cache *userUsageStatsCache) load(
	ctx context.Context,
	key userUsageStatsCacheKey,
	force bool,
	loader func() (userWindowStats, *upstreamAPIError),
) (userWindowStats, *upstreamAPIError) {
	if cache == nil {
		return loader()
	}

	cache.mutex.Lock()
	now := cache.now()
	for entryKey, entry := range cache.entries {
		if !now.Before(entry.expiresAt) {
			delete(cache.entries, entryKey)
		}
	}
	if flight, exists := cache.inflight[key]; exists {
		cache.mutex.Unlock()
		select {
		case <-flight.done:
			if force && !flight.force {
				return cache.load(ctx, key, true, loader)
			}
			return flight.stats, flight.err
		case <-ctx.Done():
			return userWindowStats{}, &upstreamAPIError{Message: "等待用户用量统计超时", Cause: ctx.Err()}
		}
	}
	if !force {
		if entry, exists := cache.entries[key]; exists {
			cache.mutex.Unlock()
			return entry.stats, entry.err
		}
	}

	flight := &userUsageStatsFlight{done: make(chan struct{}), force: force}
	cache.inflight[key] = flight
	cache.mutex.Unlock()

	stats, upstreamErr := loader()
	ttl := userUsageStatsCacheTTL
	if upstreamErr != nil {
		ttl = userUsageStatsErrorCacheTTL
	}

	cache.mutex.Lock()
	flight.stats = stats
	flight.err = upstreamErr
	cache.entries[key] = userUsageStatsCacheEntry{
		stats:     stats,
		err:       upstreamErr,
		expiresAt: cache.now().Add(ttl),
	}
	delete(cache.inflight, key)
	close(flight.done)
	cache.mutex.Unlock()
	return stats, upstreamErr
}
