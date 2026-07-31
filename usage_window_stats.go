package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	initialUsageLogPageSize   = 200
	maxUsageBoundaryRecords   = 100000
	userStatsUnavailableField = "user_window_stats_unavailable"
	resetTimePendingField     = "reset_time_pending"
)

type usageWindowDefinition struct {
	usageKey string
	stateKey string
	duration time.Duration
}

var usageWindowDefinitions = []usageWindowDefinition{
	{usageKey: "five_hour", stateKey: "5h", duration: 5 * time.Hour},
	{usageKey: "seven_day", stateKey: "7d", duration: 7 * 24 * time.Hour},
}

type usageLogPage struct {
	Items []usageLogEntry `json:"items"`
	Pages int             `json:"pages"`
}

type usageLogEntry struct {
	UserID                int64    `json:"user_id"`
	InputTokens           int64    `json:"input_tokens"`
	OutputTokens          int64    `json:"output_tokens"`
	CacheCreationTokens   int64    `json:"cache_creation_tokens"`
	CacheReadTokens       int64    `json:"cache_read_tokens"`
	TotalCost             float64  `json:"total_cost"`
	ActualCost            float64  `json:"actual_cost"`
	AccountStatsCost      *float64 `json:"account_stats_cost"`
	AccountRateMultiplier *float64 `json:"account_rate_multiplier"`
	CreatedAt             string   `json:"created_at"`
}

type userWindowStats struct {
	Requests  int64   `json:"requests"`
	Tokens    int64   `json:"tokens"`
	Cost      float64 `json:"cost"`
	QuotaCost float64 `json:"-"`
}

type userUsageAggregate struct {
	TotalRequests    *int64   `json:"total_requests"`
	TotalTokens      *int64   `json:"total_tokens"`
	TotalActualCost  *float64 `json:"total_actual_cost"`
	TotalAccountCost *float64 `json:"total_account_cost"`
}

type activeUsageWindow struct {
	progress    map[string]any
	stateKey    string
	startAt     time.Time
	provisional bool
}

func (a *app) enrichAccountUsageForUser(
	ctx context.Context,
	accountID, userID int64,
	data json.RawMessage,
	now time.Time,
	allowProvisionalWindows bool,
	force bool,
) json.RawMessage {
	if userID <= 0 {
		return data
	}

	payload, activeWindows, ok := resolveAccountUsageWindows(data, now, allowProvisionalWindows)
	if !ok {
		return data
	}
	if len(activeWindows) == 0 {
		return marshalUsagePayload(payload, data)
	}

	for _, window := range activeWindows {
		if window.provisional {
			delete(window.progress, "user_window_stats")
			delete(window.progress, userStatsUnavailableField)
			delete(window.progress, "user_utilization")
			delete(window.progress, "user_utilization_estimated")
			continue
		}
		cacheKey := userUsageStatsCacheKey{
			userID:      userID,
			accountID:   accountID,
			windowKey:   window.stateKey,
			windowStart: window.startAt.Unix(),
		}
		stats, upstreamErr := a.userUsageCache.load(ctx, cacheKey, force, func() (userWindowStats, *upstreamAPIError) {
			return a.fetchUserWindowStats(ctx, userID, accountID, window.startAt, now, force)
		})
		if upstreamErr != nil {
			log.Printf(
				"user usage window stats failed: user_id=%d account_id=%d start_at=%s end_at=%s: %v",
				userID,
				accountID,
				window.startAt.Format(time.RFC3339),
				now.Format(time.RFC3339),
				upstreamErr,
			)
			window.progress[userStatsUnavailableField] = true
			delete(window.progress, "user_window_stats")
			delete(window.progress, "user_utilization")
			delete(window.progress, "user_utilization_estimated")
			continue
		}
		delete(window.progress, userStatsUnavailableField)
		window.progress["user_window_stats"] = stats

		accountUtilization, hasUtilization := numberValue(window.progress["utilization"])
		accountCost, hasAccountCost := usageWindowAccountCost(window.progress)
		if hasUtilization && hasAccountCost && accountCost > 0 && accountUtilization >= 0 && stats.QuotaCost >= 0 {
			userUtilization := accountUtilization * stats.QuotaCost / accountCost
			if userUtilization < 0 {
				userUtilization = 0
			}
			if userUtilization > 100 {
				userUtilization = 100
			}
			window.progress["user_utilization"] = userUtilization
			window.progress["user_utilization_estimated"] = true
		}
	}

	return marshalUsagePayload(payload, data)
}

func resolveAccountUsageWindows(
	data json.RawMessage,
	now time.Time,
	allowProvisionalWindows bool,
) (map[string]any, []activeUsageWindow, bool) {
	if len(data) == 0 {
		return nil, nil, false
	}
	now = now.UTC()

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil || payload == nil {
		return nil, nil, false
	}

	activeWindows := make([]activeUsageWindow, 0, len(usageWindowDefinitions))
	for _, definition := range usageWindowDefinitions {
		progress, ok := payload[definition.usageKey].(map[string]any)
		if !ok || progress == nil {
			continue
		}
		delete(progress, "utilization_pending_confirmation")
		resetAt, hasActiveReset := activeUsageWindowReset(progress, definition.duration, now)
		if !hasActiveReset {
			if !allowProvisionalWindows {
				continue
			}
			startAt := now.Add(-definition.duration)
			progress["window_start_at"] = startAt.Format(time.RFC3339)
			progress[resetTimePendingField] = true
			activeWindows = append(activeWindows, activeUsageWindow{
				progress:    progress,
				stateKey:    definition.stateKey,
				startAt:     startAt,
				provisional: true,
			})
			continue
		}

		startAt := resetAt.Add(-definition.duration)
		delete(progress, resetTimePendingField)
		progress["resets_at"] = resetAt.Format(time.RFC3339)
		progress["remaining_seconds"] = int64(math.Ceil(resetAt.Sub(now).Seconds()))
		progress["window_start_at"] = startAt.Format(time.RFC3339)
		activeWindows = append(activeWindows, activeUsageWindow{
			progress: progress,
			stateKey: definition.stateKey,
			startAt:  startAt,
		})
	}

	return payload, activeWindows, true
}

// activeUsageWindowReset accepts the absolute reset time when present and falls
// back to the provider's remaining duration. The window start is always derived
// as resetAt-windowDuration, so no local reset-window state is required.
func activeUsageWindowReset(progress map[string]any, duration time.Duration, now time.Time) (time.Time, bool) {
	if resetAt, ok := parseUsageTimestamp(progress["resets_at"]); ok {
		resetAt = resetAt.UTC()
		startAt := resetAt.Add(-duration)
		if resetAt.After(now) && !startAt.After(now) {
			return resetAt, true
		}
	}

	remainingSeconds, ok := numberValue(progress["remaining_seconds"])
	if !ok || remainingSeconds <= 0 || remainingSeconds > duration.Seconds() {
		return time.Time{}, false
	}
	resetAt := now.Add(time.Duration(math.Ceil(remainingSeconds)) * time.Second).UTC()
	return resetAt, true
}

// fetchUserWindowStats uses Sub2API's aggregate endpoint for the bulk of a
// window. Since that endpoint accepts calendar dates instead of timestamps, it
// reads only the shorter side of the first partial UTC day from usage details
// and adds or subtracts that boundary. At most one partial day is paged.
func (a *app) fetchUserWindowStats(
	ctx context.Context,
	userID, accountID int64,
	startAt, endAt time.Time,
	force bool,
) (userWindowStats, *upstreamAPIError) {
	if userID <= 0 || accountID <= 0 || startAt.IsZero() || !endAt.After(startAt) {
		return userWindowStats{}, nil
	}
	startAt = startAt.UTC()
	endAt = endAt.UTC()
	startDay := utcDayStart(startAt)
	nextDay := startDay.AddDate(0, 0, 1)
	boundaryEnd := nextDay
	if endAt.Before(boundaryEnd) {
		boundaryEnd = endAt
	}

	if startAt.Equal(startDay) {
		return a.fetchAggregatedUserUsageStats(ctx, userID, accountID, startDay, endAt, force)
	}

	prefixDuration := startAt.Sub(startDay)
	suffixDuration := boundaryEnd.Sub(startAt)
	if prefixDuration <= suffixDuration {
		total, upstreamErr := a.fetchAggregatedUserUsageStats(ctx, userID, accountID, startDay, endAt, force)
		if upstreamErr != nil {
			return userWindowStats{}, upstreamErr
		}
		excluded, upstreamErr := a.fetchUserUsageBoundary(ctx, userID, accountID, startDay, startAt, "asc")
		if upstreamErr != nil {
			return userWindowStats{}, upstreamErr
		}
		return subtractUserWindowStats(total, excluded)
	}

	total := userWindowStats{}
	if nextDay.Before(endAt) {
		var upstreamErr *upstreamAPIError
		total, upstreamErr = a.fetchAggregatedUserUsageStats(ctx, userID, accountID, nextDay, endAt, force)
		if upstreamErr != nil {
			return userWindowStats{}, upstreamErr
		}
	}
	boundary, upstreamErr := a.fetchUserUsageBoundary(ctx, userID, accountID, startAt, boundaryEnd, "desc")
	if upstreamErr != nil {
		return userWindowStats{}, upstreamErr
	}
	return addUserWindowStats(total, boundary)
}

func (a *app) fetchAggregatedUserUsageStats(
	ctx context.Context,
	userID, accountID int64,
	startAt, endAt time.Time,
	force bool,
) (userWindowStats, *upstreamAPIError) {
	if !endAt.After(startAt) {
		return userWindowStats{}, nil
	}
	lastIncluded := endAt.UTC().Add(-time.Nanosecond)
	query := url.Values{
		"user_id":    {strconv.FormatInt(userID, 10)},
		"account_id": {strconv.FormatInt(accountID, 10)},
		"start_date": {startAt.UTC().Format("2006-01-02")},
		"end_date":   {lastIncluded.Format("2006-01-02")},
		"timezone":   {"UTC"},
	}
	if force {
		query.Set("nocache", "true")
	}
	var aggregate userUsageAggregate
	if upstreamErr := a.doAdminRequest(ctx, http.MethodGet, "/admin/usage/stats", query, &aggregate); upstreamErr != nil {
		return userWindowStats{}, upstreamErr
	}
	return aggregate.toUserWindowStats()
}

func (a *app) fetchUserUsageBoundary(
	ctx context.Context,
	userID, accountID int64,
	startAt, endAt time.Time,
	sortOrder string,
) (userWindowStats, *upstreamAPIError) {
	if !endAt.After(startAt) {
		return userWindowStats{}, nil
	}
	pageSize := initialUsageLogPageSize
	for {
		stats, upstreamErr := a.fetchUserUsageBoundaryWithPageSize(
			ctx,
			userID,
			accountID,
			startAt.UTC(),
			endAt.UTC(),
			sortOrder,
			pageSize,
		)
		if upstreamErr == nil || !isUpstreamResponseTooLarge(upstreamErr) || pageSize == 1 {
			return stats, upstreamErr
		}
		pageSize /= 2
		if pageSize < 1 {
			pageSize = 1
		}
		log.Printf(
			"usage boundary response too large; retrying: user_id=%d account_id=%d page_size=%d",
			userID,
			accountID,
			pageSize,
		)
	}
}

func (a *app) fetchUserUsageBoundaryWithPageSize(
	ctx context.Context,
	userID, accountID int64,
	startAt, endAt time.Time,
	sortOrder string,
	pageSize int,
) (userWindowStats, *upstreamAPIError) {
	if pageSize <= 0 {
		pageSize = 1
	}
	day := utcDayStart(startAt)

	query := url.Values{
		"page_size":  {strconv.Itoa(pageSize)},
		"user_id":    {strconv.FormatInt(userID, 10)},
		"account_id": {strconv.FormatInt(accountID, 10)},
		"start_date": {day.Format("2006-01-02")},
		"end_date":   {day.Format("2006-01-02")},
		"timezone":   {"UTC"},
		"sort_by":    {"created_at"},
		"sort_order": {sortOrder},
	}

	stats := userWindowStats{}
	maxPages := (maxUsageBoundaryRecords + pageSize - 1) / pageSize
	for page := 1; page <= maxPages; page++ {
		query.Set("page", strconv.Itoa(page))
		var result usageLogPage
		if upstreamErr := a.doAdminRequest(ctx, http.MethodGet, "/admin/usage", query, &result); upstreamErr != nil {
			return userWindowStats{}, upstreamErr
		}
		for _, entry := range result.Items {
			createdAt, err := time.Parse(time.RFC3339Nano, entry.CreatedAt)
			if err != nil {
				return userWindowStats{}, &upstreamAPIError{Message: "Sub2API 用户用量明细时间无效", Cause: err}
			}
			createdAt = createdAt.UTC()
			if sortOrder == "asc" && !createdAt.Before(endAt) {
				return stats, nil
			}
			if sortOrder == "desc" && createdAt.Before(startAt) {
				return stats, nil
			}
			if entry.UserID == userID && !createdAt.Before(startAt) && createdAt.Before(endAt) {
				if upstreamErr := accumulateUserUsage(&stats, entry); upstreamErr != nil {
					return userWindowStats{}, upstreamErr
				}
			}
		}
		if result.Pages <= page || len(result.Items) == 0 {
			return stats, nil
		}
	}
	return userWindowStats{}, &upstreamAPIError{Message: "Sub2API 用户用量边界明细超过安全上限"}
}

func (aggregate userUsageAggregate) toUserWindowStats() (userWindowStats, *upstreamAPIError) {
	if aggregate.TotalRequests == nil || aggregate.TotalTokens == nil ||
		aggregate.TotalActualCost == nil || aggregate.TotalAccountCost == nil {
		return userWindowStats{}, &upstreamAPIError{Message: "Sub2API 用户用量聚合字段不完整"}
	}
	if *aggregate.TotalRequests < 0 || *aggregate.TotalTokens < 0 ||
		!isNonNegativeFinite(*aggregate.TotalActualCost) || !isNonNegativeFinite(*aggregate.TotalAccountCost) {
		return userWindowStats{}, &upstreamAPIError{Message: "Sub2API 用户用量聚合数值无效"}
	}
	return userWindowStats{
		Requests:  *aggregate.TotalRequests,
		Tokens:    *aggregate.TotalTokens,
		Cost:      *aggregate.TotalActualCost,
		QuotaCost: *aggregate.TotalAccountCost,
	}, nil
}

func accumulateUserUsage(stats *userWindowStats, entry usageLogEntry) *upstreamAPIError {
	if entry.InputTokens < 0 || entry.OutputTokens < 0 || entry.CacheCreationTokens < 0 || entry.CacheReadTokens < 0 ||
		!isNonNegativeFinite(entry.TotalCost) || !isNonNegativeFinite(entry.ActualCost) {
		return &upstreamAPIError{Message: "Sub2API 用户用量明细数值无效"}
	}
	tokenCount, ok := addNonNegativeInt64(
		entry.InputTokens,
		entry.OutputTokens,
		entry.CacheCreationTokens,
		entry.CacheReadTokens,
	)
	if !ok {
		return &upstreamAPIError{Message: "Sub2API 用户用量明细令牌数溢出"}
	}
	accountCost := entry.TotalCost
	if entry.AccountStatsCost != nil {
		if !isNonNegativeFinite(*entry.AccountStatsCost) {
			return &upstreamAPIError{Message: "Sub2API 用户用量账号成本无效"}
		}
		accountCost = *entry.AccountStatsCost
	}
	accountMultiplier := 1.0
	if entry.AccountRateMultiplier != nil {
		if !isNonNegativeFinite(*entry.AccountRateMultiplier) {
			return &upstreamAPIError{Message: "Sub2API 用户用量账号倍率无效"}
		}
		accountMultiplier = *entry.AccountRateMultiplier
	}
	quotaCost := accountCost * accountMultiplier
	if !isNonNegativeFinite(quotaCost) {
		return &upstreamAPIError{Message: "Sub2API 用户用量账号成本无效"}
	}

	requests, ok := addNonNegativeInt64(stats.Requests, 1)
	if !ok {
		return &upstreamAPIError{Message: "Sub2API 用户用量请求数溢出"}
	}
	tokens, ok := addNonNegativeInt64(stats.Tokens, tokenCount)
	if !ok {
		return &upstreamAPIError{Message: "Sub2API 用户用量令牌数溢出"}
	}
	cost := stats.Cost + entry.ActualCost
	quotaCostTotal := stats.QuotaCost + quotaCost
	if !isNonNegativeFinite(cost) || !isNonNegativeFinite(quotaCostTotal) {
		return &upstreamAPIError{Message: "Sub2API 用户用量金额溢出"}
	}

	stats.Requests = requests
	stats.Tokens = tokens
	stats.Cost = cost
	stats.QuotaCost = quotaCostTotal
	return nil
}

func addUserWindowStats(left, right userWindowStats) (userWindowStats, *upstreamAPIError) {
	requests, requestsOK := addNonNegativeInt64(left.Requests, right.Requests)
	tokens, tokensOK := addNonNegativeInt64(left.Tokens, right.Tokens)
	cost := left.Cost + right.Cost
	quotaCost := left.QuotaCost + right.QuotaCost
	if !requestsOK || !tokensOK || !isNonNegativeFinite(cost) || !isNonNegativeFinite(quotaCost) {
		return userWindowStats{}, &upstreamAPIError{Message: "Sub2API 用户用量合计数值无效"}
	}
	return userWindowStats{
		Requests:  requests,
		Tokens:    tokens,
		Cost:      cost,
		QuotaCost: quotaCost,
	}, nil
}

func subtractUserWindowStats(total, excluded userWindowStats) (userWindowStats, *upstreamAPIError) {
	const costTolerance = 1e-9
	if excluded.Requests > total.Requests || excluded.Tokens > total.Tokens ||
		excluded.Cost > total.Cost+costTolerance || excluded.QuotaCost > total.QuotaCost+costTolerance {
		return userWindowStats{}, &upstreamAPIError{Message: "Sub2API 用户用量聚合结果不一致"}
	}
	result := userWindowStats{
		Requests:  total.Requests - excluded.Requests,
		Tokens:    total.Tokens - excluded.Tokens,
		Cost:      total.Cost - excluded.Cost,
		QuotaCost: total.QuotaCost - excluded.QuotaCost,
	}
	if result.Cost < 0 && result.Cost >= -costTolerance {
		result.Cost = 0
	}
	if result.QuotaCost < 0 && result.QuotaCost >= -costTolerance {
		result.QuotaCost = 0
	}
	return result, nil
}

func isNonNegativeFinite(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func addNonNegativeInt64(values ...int64) (int64, bool) {
	const maxInt64 = int64(^uint64(0) >> 1)
	total := int64(0)
	for _, value := range values {
		if value < 0 || total > maxInt64-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

func utcDayStart(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func isUpstreamResponseTooLarge(upstreamErr *upstreamAPIError) bool {
	return upstreamErr != nil && upstreamErr.Message == "Sub2API 响应过大"
}

func usageWindowAccountCost(progress map[string]any) (float64, bool) {
	windowStats, ok := progress["window_stats"].(map[string]any)
	if !ok || windowStats == nil {
		return 0, false
	}
	return numberValue(windowStats["cost"])
}

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func parseUsageTimestamp(value any) (time.Time, bool) {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func marshalUsagePayload(payload map[string]any, fallback json.RawMessage) json.RawMessage {
	data, err := json.Marshal(payload)
	if err != nil {
		return fallback
	}
	return data
}
