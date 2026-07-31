package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUserUsageStatsCacheCachesAndForceRefreshes(t *testing.T) {
	cache := newUserUsageStatsCache()
	currentTime := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return currentTime }
	key := userUsageStatsCacheKey{userID: 2, accountID: 14, windowKey: "5h", windowStart: 123}

	loads := 0
	loader := func() (userWindowStats, *upstreamAPIError) {
		loads++
		return userWindowStats{Requests: int64(loads)}, nil
	}
	first, upstreamErr := cache.load(context.Background(), key, false, loader)
	if upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	second, upstreamErr := cache.load(context.Background(), key, false, loader)
	if upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if first.Requests != 1 || second.Requests != 1 || loads != 1 {
		t.Fatalf("first=%+v second=%+v loads=%d", first, second, loads)
	}

	forced, upstreamErr := cache.load(context.Background(), key, true, loader)
	if upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if forced.Requests != 2 || loads != 2 {
		t.Fatalf("forced=%+v loads=%d", forced, loads)
	}
}

func TestUserUsageStatsCacheCoalescesConcurrentLoads(t *testing.T) {
	cache := newUserUsageStatsCache()
	key := userUsageStatsCacheKey{userID: 2, accountID: 14, windowKey: "7d", windowStart: 456}
	started := make(chan struct{})
	release := make(chan struct{})
	var loads atomic.Int32
	loader := func() (userWindowStats, *upstreamAPIError) {
		loads.Add(1)
		close(started)
		<-release
		return userWindowStats{Requests: 3}, nil
	}

	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	results := make(chan userWindowStats, 2)
	go func() {
		defer waitGroup.Done()
		stats, _ := cache.load(context.Background(), key, false, loader)
		results <- stats
	}()
	<-started
	go func() {
		defer waitGroup.Done()
		stats, _ := cache.load(context.Background(), key, false, loader)
		results <- stats
	}()
	close(release)
	waitGroup.Wait()
	close(results)

	for stats := range results {
		if stats.Requests != 3 {
			t.Fatalf("stats = %+v", stats)
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}
}

func TestUserUsageStatsCacheForceWaitsThenReloadsNormalFlight(t *testing.T) {
	cache := newUserUsageStatsCache()
	key := userUsageStatsCacheKey{userID: 2, accountID: 14, windowKey: "5h", windowStart: 789}
	started := make(chan struct{})
	release := make(chan struct{})
	var loads atomic.Int32
	loader := func() (userWindowStats, *upstreamAPIError) {
		loadNumber := loads.Add(1)
		if loadNumber == 1 {
			close(started)
			<-release
		}
		return userWindowStats{Requests: int64(loadNumber)}, nil
	}

	normalResult := make(chan userWindowStats, 1)
	go func() {
		stats, _ := cache.load(context.Background(), key, false, loader)
		normalResult <- stats
	}()
	<-started

	forcedResult := make(chan userWindowStats, 1)
	go func() {
		stats, _ := cache.load(context.Background(), key, true, loader)
		forcedResult <- stats
	}()
	close(release)

	if stats := <-normalResult; stats.Requests != 1 {
		t.Fatalf("normal stats = %+v", stats)
	}
	if stats := <-forcedResult; stats.Requests != 2 {
		t.Fatalf("forced stats = %+v", stats)
	}
	if loads.Load() != 2 {
		t.Fatalf("loads = %d, want 2", loads.Load())
	}
}

func TestUserUsageStatsCacheRemovesExpiredEntries(t *testing.T) {
	cache := newUserUsageStatsCache()
	currentTime := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return currentTime }
	oldKey := userUsageStatsCacheKey{userID: 2, accountID: 14, windowKey: "5h", windowStart: 1}
	newKey := userUsageStatsCacheKey{userID: 2, accountID: 14, windowKey: "5h", windowStart: 2}
	loader := func() (userWindowStats, *upstreamAPIError) {
		return userWindowStats{Requests: 1}, nil
	}

	if _, upstreamErr := cache.load(context.Background(), oldKey, false, loader); upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	currentTime = currentTime.Add(userUsageStatsCacheTTL)
	if _, upstreamErr := cache.load(context.Background(), newKey, false, loader); upstreamErr != nil {
		t.Fatal(upstreamErr)
	}

	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if _, exists := cache.entries[oldKey]; exists {
		t.Fatal("expired entry was not removed")
	}
}
