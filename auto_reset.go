package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	autoResetLeadTime             = 10 * time.Minute
	autoResetAccountScanInterval  = time.Hour
	autoResetAccountScanRetry     = 30 * time.Minute
	autoResetInitialQuerySpacing  = 30 * time.Second
	autoResetQuotaRefreshInterval = 6 * time.Hour
	autoResetQuotaRetryInterval   = 30 * time.Minute
	autoResetPostResetCheck       = 15 * time.Minute
)

type autoResetCredit struct {
	ExpiresAt string `json:"expires_at"`
}

type autoResetCreditSummary struct {
	AvailableCount int               `json:"available_count"`
	Credits        []autoResetCredit `json:"credits"`
}

type autoResetQuota struct {
	RateLimitResetCredits *autoResetCreditSummary `json:"rate_limit_reset_credits"`
}

type autoResetResult struct {
	WindowsReset int `json:"windows_reset"`
}

type autoResetAccountState struct {
	nextQuotaCheck time.Time
	resetAt        time.Time
}

type autoResetWorker struct {
	app           *app
	accounts      map[int64]*autoResetAccountState
	nextDiscovery time.Time
	now           func() time.Time
}

func newAutoResetWorker(application *app) *autoResetWorker {
	return &autoResetWorker{
		app:      application,
		accounts: make(map[int64]*autoResetAccountState),
		now:      time.Now,
	}
}

func (a *app) runAutoResetCredits(ctx context.Context) {
	worker := newAutoResetWorker(a)
	log.Printf(
		"automatic reset credits enabled: account scan=%s quota refresh=%s cache=%s lead=%s",
		autoResetAccountScanInterval,
		autoResetQuotaRefreshInterval,
		accountQuotaCacheTTL,
		autoResetLeadTime,
	)

	for {
		now := worker.now()
		nextWake := worker.step(ctx, now)
		delay := time.Until(nextWake)
		if delay < time.Second {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
}

func (worker *autoResetWorker) step(ctx context.Context, now time.Time) time.Time {
	if worker.nextDiscovery.IsZero() || !now.Before(worker.nextDiscovery) {
		worker.discoverAccounts(ctx, now)
	}

	accountIDs := make([]int64, 0, len(worker.accounts))
	for accountID := range worker.accounts {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Slice(accountIDs, func(left, right int) bool {
		return accountIDs[left] < accountIDs[right]
	})

	for _, accountID := range accountIDs {
		if ctx.Err() != nil {
			break
		}
		state := worker.accounts[accountID]
		switch {
		case !state.resetAt.IsZero() && !now.Before(state.resetAt):
			worker.consumeExpiringCredit(ctx, accountID, state, now)
		case state.nextQuotaCheck.IsZero() || !now.Before(state.nextQuotaCheck):
			worker.refreshQuota(ctx, accountID, state, now)
		}
	}
	return worker.nextWake(now)
}

func (worker *autoResetWorker) discoverAccounts(ctx context.Context, now time.Time) {
	accounts, upstreamErr := worker.app.listAutoResetAccounts(ctx)
	if upstreamErr != nil {
		log.Printf("automatic reset account scan failed: %v", upstreamErr)
		worker.nextDiscovery = now.Add(autoResetAccountScanRetry)
		return
	}

	seen := make(map[int64]struct{}, len(accounts))
	newAccountIndex := 0
	for _, account := range accounts {
		if account.ID <= 0 ||
			account.Platform != "openai" ||
			account.Type != "oauth" ||
			account.Status != "active" ||
			account.ParentAccountID != nil {
			continue
		}
		seen[account.ID] = struct{}{}
		if _, exists := worker.accounts[account.ID]; !exists {
			worker.accounts[account.ID] = &autoResetAccountState{
				nextQuotaCheck: now.Add(time.Duration(newAccountIndex) * autoResetInitialQuerySpacing),
			}
			newAccountIndex++
		}
	}
	for accountID := range worker.accounts {
		if _, exists := seen[accountID]; !exists {
			delete(worker.accounts, accountID)
		}
	}
	worker.nextDiscovery = now.Add(autoResetAccountScanInterval)
}

func (worker *autoResetWorker) refreshQuota(
	ctx context.Context,
	accountID int64,
	state *autoResetAccountState,
	now time.Time,
) {
	var quota autoResetQuota
	if upstreamErr := worker.app.queryOpenAIQuota(ctx, accountID, &quota); upstreamErr != nil {
		log.Printf("automatic reset quota query failed for account %d: %v", accountID, upstreamErr)
		state.resetAt = time.Time{}
		state.nextQuotaCheck = now.Add(autoResetQuotaRetryInterval)
		return
	}
	worker.applyQuotaSchedule(state, quota, now)
}

func (worker *autoResetWorker) consumeExpiringCredit(
	ctx context.Context,
	accountID int64,
	state *autoResetAccountState,
	now time.Time,
) {
	var quota autoResetQuota
	if upstreamErr := worker.app.queryOpenAIQuota(ctx, accountID, &quota); upstreamErr != nil {
		log.Printf("automatic reset quota verification failed for account %d: %v", accountID, upstreamErr)
		state.resetAt = time.Time{}
		state.nextQuotaCheck = now.Add(autoResetQuotaRetryInterval)
		return
	}

	expiresAt, exists := earliestFutureCreditExpiry(quota, now)
	if !exists || expiresAt.After(now.Add(autoResetLeadTime)) {
		worker.applyQuotaSchedule(state, quota, now)
		return
	}

	state.resetAt = time.Time{}
	state.nextQuotaCheck = now.Add(autoResetQuotaRetryInterval)
	var result autoResetResult
	if upstreamErr := worker.app.resetOpenAIQuota(ctx, accountID, &result); upstreamErr != nil {
		log.Printf(
			"automatic reset failed for account %d before credit expiry %s: %v",
			accountID,
			expiresAt.Format(time.RFC3339),
			upstreamErr,
		)
		return
	}
	state.nextQuotaCheck = now.Add(autoResetPostResetCheck)
	log.Printf(
		"automatic reset succeeded for account %d before credit expiry %s: windows_reset=%d",
		accountID,
		expiresAt.Format(time.RFC3339),
		result.WindowsReset,
	)
}

func (worker *autoResetWorker) applyQuotaSchedule(
	state *autoResetAccountState,
	quota autoResetQuota,
	now time.Time,
) {
	state.nextQuotaCheck = now.Add(autoResetQuotaRefreshInterval)
	expiresAt, exists := earliestFutureCreditExpiry(quota, now)
	if !exists {
		state.resetAt = time.Time{}
		return
	}

	resetAt := expiresAt.Add(-autoResetLeadTime)
	if resetAt.Before(now) {
		resetAt = now
	}
	state.resetAt = resetAt
}

func (worker *autoResetWorker) nextWake(now time.Time) time.Time {
	nextWake := worker.nextDiscovery
	for _, state := range worker.accounts {
		for _, candidate := range []time.Time{state.nextQuotaCheck, state.resetAt} {
			if candidate.IsZero() {
				continue
			}
			if nextWake.IsZero() || candidate.Before(nextWake) {
				nextWake = candidate
			}
		}
	}
	if nextWake.IsZero() {
		return now.Add(autoResetAccountScanRetry)
	}
	return nextWake
}

func earliestFutureCreditExpiry(quota autoResetQuota, now time.Time) (time.Time, bool) {
	credits := quota.RateLimitResetCredits
	if credits == nil || credits.AvailableCount <= 0 {
		return time.Time{}, false
	}

	var earliest time.Time
	for _, credit := range credits.Credits {
		value := strings.TrimSpace(credit.ExpiresAt)
		if value == "" {
			continue
		}
		expiresAt, err := time.Parse(time.RFC3339, value)
		if err != nil || !expiresAt.After(now) {
			continue
		}
		if earliest.IsZero() || expiresAt.Before(earliest) {
			earliest = expiresAt
		}
	}
	return earliest, !earliest.IsZero()
}

func (a *app) listAutoResetAccounts(ctx context.Context) ([]accountView, *upstreamAPIError) {
	accounts := make([]accountView, 0)
	for page := 1; ; page++ {
		query := url.Values{
			"page":       {strconv.Itoa(page)},
			"page_size":  {strconv.Itoa(accountsPageSize)},
			"platform":   {"openai"},
			"type":       {"oauth"},
			"status":     {"active"},
			"sort_by":    {"id"},
			"sort_order": {"asc"},
			"lite":       {"true"},
		}
		var result accountPage
		if upstreamErr := a.doAdminRequest(ctx, http.MethodGet, "/admin/accounts", query, &result); upstreamErr != nil {
			return nil, upstreamErr
		}
		accounts = append(accounts, result.Items...)
		if result.Pages <= page || len(result.Items) == 0 {
			break
		}
	}
	return accounts, nil
}
