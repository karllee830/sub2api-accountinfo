package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	accountQuotaCacheTTL      = 15 * time.Minute
	accountQuotaErrorCacheTTL = 5 * time.Minute
)

type accountQuotaCacheEntry struct {
	data      json.RawMessage
	err       *upstreamAPIError
	expiresAt time.Time
}

type accountQuotaFlight struct {
	done    chan struct{}
	version uint64
	data    json.RawMessage
	err     *upstreamAPIError
}

type accountQuotaCache struct {
	mutex    sync.Mutex
	entries  map[int64]accountQuotaCacheEntry
	inflight map[int64]*accountQuotaFlight
	versions map[int64]uint64
	now      func() time.Time
}

func newAccountQuotaCache() *accountQuotaCache {
	return &accountQuotaCache{
		entries:  make(map[int64]accountQuotaCacheEntry),
		inflight: make(map[int64]*accountQuotaFlight),
		versions: make(map[int64]uint64),
		now:      time.Now,
	}
}

func (cache *accountQuotaCache) load(
	ctx context.Context,
	accountID int64,
	loader func() (json.RawMessage, *upstreamAPIError),
) (json.RawMessage, *upstreamAPIError) {
	cache.mutex.Lock()
	now := cache.now()
	if entry, exists := cache.entries[accountID]; exists && now.Before(entry.expiresAt) {
		cache.mutex.Unlock()
		return cloneRawMessage(entry.data), entry.err
	}
	version := cache.versions[accountID]
	if flight, exists := cache.inflight[accountID]; exists && flight.version == version {
		cache.mutex.Unlock()
		select {
		case <-flight.done:
			return cloneRawMessage(flight.data), flight.err
		case <-ctx.Done():
			return nil, &upstreamAPIError{Message: "等待 Sub2API 配额请求超时", Cause: ctx.Err()}
		}
	}
	flight := &accountQuotaFlight{done: make(chan struct{}), version: version}
	cache.inflight[accountID] = flight
	cache.mutex.Unlock()

	data, upstreamErr := loader()
	ttl := accountQuotaCacheTTL
	if upstreamErr != nil {
		ttl = accountQuotaErrorCacheTTL
	}

	cache.mutex.Lock()
	flight.data = cloneRawMessage(data)
	flight.err = upstreamErr
	if cache.versions[accountID] == flight.version {
		cache.entries[accountID] = accountQuotaCacheEntry{
			data:      cloneRawMessage(data),
			err:       upstreamErr,
			expiresAt: cache.now().Add(ttl),
		}
	}
	if cache.inflight[accountID] == flight {
		delete(cache.inflight, accountID)
	}
	close(flight.done)
	cache.mutex.Unlock()
	return cloneRawMessage(data), upstreamErr
}

func (cache *accountQuotaCache) invalidate(accountID int64) {
	cache.mutex.Lock()
	cache.versions[accountID]++
	delete(cache.entries, accountID)
	cache.mutex.Unlock()
}

func cloneRawMessage(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), data...)
}

func (a *app) queryOpenAIQuota(ctx context.Context, accountID int64, out any) *upstreamAPIError {
	loader := func() (json.RawMessage, *upstreamAPIError) {
		return a.fetchOpenAIQuota(ctx, accountID)
	}
	var data json.RawMessage
	var upstreamErr *upstreamAPIError
	if a.config.autoResetCredits {
		data, upstreamErr = a.quotaCache.load(ctx, accountID, loader)
	} else {
		data, upstreamErr = loader()
	}
	if upstreamErr != nil {
		return upstreamErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &upstreamAPIError{Message: "无法解析 Sub2API 配额响应", Cause: err}
	}
	return nil
}

func (a *app) fetchOpenAIQuota(ctx context.Context, accountID int64) (json.RawMessage, *upstreamAPIError) {
	var payload json.RawMessage
	upstreamErr := a.doAdminRequest(
		ctx,
		http.MethodGet,
		"/admin/openai/accounts/"+strconv.FormatInt(accountID, 10)+"/quota",
		nil,
		&payload,
	)
	return payload, upstreamErr
}

func (a *app) resetOpenAIQuota(ctx context.Context, accountID int64, out any) *upstreamAPIError {
	upstreamErr := a.doAdminRequest(
		ctx,
		http.MethodPost,
		"/admin/openai/accounts/"+strconv.FormatInt(accountID, 10)+"/reset-quota",
		nil,
		out,
	)
	if upstreamErr == nil {
		a.quotaCache.invalidate(accountID)
	}
	return upstreamErr
}
