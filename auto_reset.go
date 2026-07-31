package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	autoResetLeadTime             = 10 * time.Minute
	autoResetAccountScanInterval  = 15 * time.Minute
	autoResetAccountScanRetry     = 30 * time.Minute
	autoResetInitialQuerySpacing  = 30 * time.Second
	autoResetQuotaRefreshInterval = 6 * time.Hour
	autoResetQuotaRetryInterval   = 30 * time.Minute
	autoResetVerificationRetry    = 5 * time.Minute
	autoResetPostResetCheck       = 2 * time.Minute
	autoResetDueConcurrency       = 8
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

type autoResetAccount struct {
	ID              int64          `json:"id"`
	Platform        string         `json:"platform"`
	Type            string         `json:"type"`
	Status          string         `json:"status"`
	ParentAccountID *int64         `json:"parent_account_id,omitempty"`
	Credentials     map[string]any `json:"credentials,omitempty"`
}

type autoResetAccountPage struct {
	Items []autoResetAccount `json:"items"`
	Pages int                `json:"pages"`
}

type autoResetAccountState struct {
	nextQuotaCheck         time.Time
	resetAt                time.Time
	expiresAt              time.Time
	lastAttemptFingerprint string
}

type autoResetSchedule struct {
	AccountID int64     `json:"account_id"`
	ResetAt   time.Time `json:"reset_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type autoResetScheduleStore struct {
	mutex     sync.RWMutex
	schedules []autoResetSchedule
}

func newAutoResetScheduleStore() *autoResetScheduleStore {
	return &autoResetScheduleStore{schedules: []autoResetSchedule{}}
}

func (store *autoResetScheduleStore) replace(schedules []autoResetSchedule) {
	if store == nil {
		return
	}
	copyOfSchedules := append([]autoResetSchedule(nil), schedules...)
	sort.Slice(copyOfSchedules, func(left, right int) bool {
		return copyOfSchedules[left].ResetAt.Before(copyOfSchedules[right].ResetAt) ||
			(copyOfSchedules[left].ResetAt.Equal(copyOfSchedules[right].ResetAt) && copyOfSchedules[left].AccountID < copyOfSchedules[right].AccountID)
	})
	store.mutex.Lock()
	store.schedules = copyOfSchedules
	store.mutex.Unlock()
}

func (store *autoResetScheduleStore) forAccounts(accountIDs map[int64]struct{}) []autoResetSchedule {
	if store == nil {
		return []autoResetSchedule{}
	}
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	schedules := make([]autoResetSchedule, 0, len(store.schedules))
	for _, schedule := range store.schedules {
		if _, visible := accountIDs[schedule.AccountID]; visible {
			schedules = append(schedules, schedule)
		}
	}
	return schedules
}

type autoResetTask struct {
	accountID int64
	state     *autoResetAccountState
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
	defer a.autoResetPlans.replace(nil)
	log.Printf(
		"automatic reset credits enabled: account/usage scan=%s quota refresh=%s cache=%s lead=%s",
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

	dueResets := make([]autoResetTask, 0)
	dueRefreshes := make([]autoResetTask, 0)
	for _, accountID := range accountIDs {
		state := worker.accounts[accountID]
		switch {
		case !state.resetAt.IsZero() && !now.Before(state.resetAt):
			dueResets = append(dueResets, autoResetTask{accountID: accountID, state: state})
		case state.nextQuotaCheck.IsZero() || !now.Before(state.nextQuotaCheck):
			dueRefreshes = append(dueRefreshes, autoResetTask{accountID: accountID, state: state})
		}
	}

	worker.consumeDueCredits(ctx, dueResets)
	for _, task := range dueRefreshes {
		if ctx.Err() != nil {
			break
		}
		worker.refreshQuota(ctx, task.accountID, task.state, worker.now())
		if !task.state.resetAt.IsZero() && !worker.now().Before(task.state.resetAt) {
			worker.consumeExpiringCredit(ctx, task.accountID, task.state)
		}
	}
	worker.publishSchedule()
	return worker.nextWake(now)
}

func (worker *autoResetWorker) publishSchedule() {
	if worker.app == nil || worker.app.autoResetPlans == nil {
		return
	}
	schedules := make([]autoResetSchedule, 0, len(worker.accounts))
	for accountID, state := range worker.accounts {
		if state == nil || state.resetAt.IsZero() {
			continue
		}
		schedules = append(schedules, autoResetSchedule{
			AccountID: accountID,
			ResetAt:   state.resetAt,
			ExpiresAt: state.expiresAt,
		})
	}
	worker.app.autoResetPlans.replace(schedules)
}

func (worker *autoResetWorker) consumeDueCredits(ctx context.Context, tasks []autoResetTask) {
	if len(tasks) == 0 {
		return
	}
	semaphore := make(chan struct{}, autoResetDueConcurrency)
	var waitGroup sync.WaitGroup
	for _, task := range tasks {
		task := task
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			worker.consumeExpiringCredit(ctx, task.accountID, task.state)
		}()
	}
	waitGroup.Wait()
}

func (worker *autoResetWorker) discoverAccounts(ctx context.Context, now time.Time) {
	accounts, upstreamErr := worker.app.listAutoResetAccounts(ctx)
	if upstreamErr != nil {
		log.Printf("automatic reset account scan failed: %v", upstreamErr)
		worker.nextDiscovery = now.Add(autoResetAccountScanRetry)
		return
	}

	seen := make(map[int64]struct{}, len(accounts))
	seenOpenAIAccounts := make(map[string]struct{}, len(accounts))
	newAccountIndex := 0
	for _, account := range accounts {
		if !isAutoResetAccountEligible(account) {
			continue
		}
		identity := autoResetAccountIdentity(account)
		if _, duplicate := seenOpenAIAccounts[identity]; duplicate {
			continue
		}
		seenOpenAIAccounts[identity] = struct{}{}
		seen[account.ID] = struct{}{}
		if _, exists := worker.accounts[account.ID]; !exists {
			batchIndex := newAccountIndex / autoResetDueConcurrency
			worker.accounts[account.ID] = &autoResetAccountState{
				nextQuotaCheck: now.Add(time.Duration(batchIndex) * autoResetInitialQuerySpacing),
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
) {
	scheduledExpiresAt := state.expiresAt
	account, accountErr := worker.app.loadAutoResetAccount(ctx, accountID)
	checkedAt := worker.now()
	if accountErr != nil {
		log.Printf("automatic reset account verification failed for account %d: %v", accountID, accountErr)
		worker.scheduleVerificationRetry(state, scheduledExpiresAt, checkedAt)
		return
	}
	if account.ID != accountID || !isAutoResetAccountEligible(account) {
		clearAutoResetTarget(state)
		state.nextQuotaCheck = checkedAt.Add(autoResetQuotaRefreshInterval)
		log.Printf("automatic reset skipped account %d because it is no longer an active OpenAI OAuth parent account", accountID)
		return
	}

	var quota autoResetQuota
	var quotaErr *upstreamAPIError
	var resetErr *upstreamAPIError
	var resetResult autoResetResult
	var currentExpiresAt time.Time
	var duplicateSnapshot bool
	var resetAttempted bool

	coordinatorErr := worker.app.resetCoordinator.withIdentity(
		autoResetAccountIdentity(account),
		func(resetState *accountResetState) *upstreamAPIError {
			quotaErr = worker.app.queryOpenAIQuotaFresh(ctx, accountID, &quota)
			checkedAt = worker.now()
			if quotaErr != nil {
				return nil
			}

			var exists bool
			currentExpiresAt, exists = earliestFutureCreditExpiry(quota, checkedAt)
			if !exists || currentExpiresAt.After(checkedAt.Add(autoResetLeadTime)) {
				return nil
			}

			fingerprint := resetCreditSnapshotFingerprint(quota, checkedAt)
			if state.lastAttemptFingerprint == fingerprint {
				duplicateSnapshot = true
				return nil
			}
			if recentErr := worker.app.resetCoordinator.beginAttempt(resetState); recentErr != nil {
				return recentErr
			}
			defer worker.app.resetCoordinator.finishAttempt(resetState)

			state.lastAttemptFingerprint = fingerprint
			resetAttempted = true
			resetErr = worker.app.performOpenAIQuotaReset(ctx, accountID, &resetResult)
			return nil
		},
	)
	now := checkedAt
	if now.IsZero() {
		now = worker.now()
	}

	if quotaErr != nil {
		log.Printf("automatic reset quota verification failed for account %d: %v", accountID, quotaErr)
		worker.scheduleVerificationRetry(state, scheduledExpiresAt, now)
		return
	}

	if duplicateSnapshot {
		clearAutoResetTarget(state)
		state.nextQuotaCheck = now.Add(autoResetPostResetCheck)
		log.Printf(
			"automatic reset skipped duplicate quota snapshot for account %d before credit expiry %s",
			accountID,
			currentExpiresAt.Format(time.RFC3339),
		)
		return
	}

	if coordinatorErr != nil {
		clearAutoResetTarget(state)
		state.nextQuotaCheck = now.Add(autoResetPostResetCheck)
		log.Printf(
			"automatic reset deferred for account %d after another recent reset attempt: %v",
			accountID,
			coordinatorErr,
		)
		return
	}

	if !resetAttempted {
		worker.applyQuotaSchedule(state, quota, now)
		return
	}

	clearAutoResetTarget(state)
	state.nextQuotaCheck = now.Add(autoResetPostResetCheck)
	if resetErr != nil {
		log.Printf(
			"automatic reset failed for account %d before credit expiry %s: %v",
			accountID,
			currentExpiresAt.Format(time.RFC3339),
			resetErr,
		)
		return
	}
	log.Printf(
		"automatic reset succeeded for account %d before credit expiry %s: windows_reset=%d",
		accountID,
		currentExpiresAt.Format(time.RFC3339),
		resetResult.WindowsReset,
	)
}

func (worker *autoResetWorker) scheduleVerificationRetry(
	state *autoResetAccountState,
	scheduledExpiresAt time.Time,
	now time.Time,
) {
	retryAt := now.Add(autoResetVerificationRetry)
	if scheduledExpiresAt.After(retryAt) {
		state.resetAt = retryAt
		state.nextQuotaCheck = now.Add(autoResetQuotaRefreshInterval)
		return
	}
	clearAutoResetTarget(state)
	state.nextQuotaCheck = now.Add(autoResetQuotaRetryInterval)
}

func (worker *autoResetWorker) applyQuotaSchedule(
	state *autoResetAccountState,
	quota autoResetQuota,
	now time.Time,
) {
	state.nextQuotaCheck = now.Add(autoResetQuotaRefreshInterval)
	expiresAt, exists := earliestFutureCreditExpiry(quota, now)
	if !exists {
		clearAutoResetTarget(state)
		return
	}

	resetAt := expiresAt.Add(-autoResetLeadTime)
	if resetAt.Before(now) {
		resetAt = now
	}
	state.resetAt = resetAt
	state.expiresAt = expiresAt
}

func clearAutoResetTarget(state *autoResetAccountState) {
	state.resetAt = time.Time{}
	state.expiresAt = time.Time{}
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

func resetCreditSnapshotFingerprint(quota autoResetQuota, now time.Time) string {
	credits := quota.RateLimitResetCredits
	if credits == nil {
		return "0"
	}

	expirations := make([]string, 0, len(credits.Credits))
	for _, credit := range credits.Credits {
		expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(credit.ExpiresAt))
		if err != nil || !expiresAt.After(now) {
			continue
		}
		expirations = append(expirations, expiresAt.UTC().Format(time.RFC3339Nano))
	}
	sort.Strings(expirations)
	return fmt.Sprintf("%d|%s", credits.AvailableCount, strings.Join(expirations, ","))
}

func (a *app) listAutoResetAccounts(ctx context.Context) ([]autoResetAccount, *upstreamAPIError) {
	accounts := make([]autoResetAccount, 0)
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
		var result autoResetAccountPage
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

func (a *app) loadAutoResetAccount(ctx context.Context, accountID int64) (autoResetAccount, *upstreamAPIError) {
	var account autoResetAccount
	if upstreamErr := a.doAdminRequest(
		ctx,
		http.MethodGet,
		"/admin/accounts/"+strconv.FormatInt(accountID, 10),
		nil,
		&account,
	); upstreamErr != nil {
		return autoResetAccount{}, upstreamErr
	}
	return account, nil
}

func isAutoResetAccountEligible(account autoResetAccount) bool {
	return account.ID > 0 &&
		account.Platform == "openai" &&
		account.Type == "oauth" &&
		account.Status == "active" &&
		account.ParentAccountID == nil
}

func autoResetAccountIdentity(account autoResetAccount) string {
	for _, key := range []string{"chatgpt_account_id", "organization_id"} {
		if value, ok := account.Credentials[key].(string); ok {
			if identity := strings.TrimSpace(value); identity != "" {
				return "openai:" + identity
			}
		}
	}
	return "sub2api:" + strconv.FormatInt(account.ID, 10)
}
