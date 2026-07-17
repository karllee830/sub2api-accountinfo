package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const (
	embeddedUserIDHeader = "X-Sub2API-User-ID"
	accountsPageSize     = 100
	usageConcurrency     = 4
)

var errSub2APIUnauthorized = errors.New("Sub2API user token is unauthorized")

type requestError struct {
	Status  int
	Code    int
	Message string
}

type upstreamAPIError struct {
	Status  int
	Code    string
	Message string
	Cause   error
}

func (e *upstreamAPIError) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

func (e *upstreamAPIError) Unwrap() error {
	return e.Cause
}

type upstreamEnvelope struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Reason  string          `json:"reason"`
	Data    json.RawMessage `json:"data"`
}

type currentUser struct {
	ID int64 `json:"id"`
}

type subscriptionGroup struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Platform         string `json:"platform"`
	Status           string `json:"status"`
	SubscriptionType string `json:"subscription_type"`
}

type userSubscription struct {
	ID        int64              `json:"id"`
	UserID    int64              `json:"user_id"`
	GroupID   int64              `json:"group_id"`
	Status    string             `json:"status"`
	ExpiresAt *string            `json:"expires_at"`
	Group     *subscriptionGroup `json:"group"`
}

type accountView struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Platform    string          `json:"platform"`
	Type        string          `json:"type"`
	Status      string          `json:"status"`
	Schedulable bool            `json:"schedulable"`
	GroupIDs    []int64         `json:"group_ids,omitempty"`
	Usage       json.RawMessage `json:"usage,omitempty"`
	UsageError  string          `json:"usage_error,omitempty"`
}

type accountPage struct {
	Items    []accountView `json:"items"`
	Total    int64         `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Pages    int           `json:"pages"`
}

type dashboardGroup struct {
	ID               int64         `json:"id"`
	Name             string        `json:"name"`
	Platform         string        `json:"platform"`
	Status           string        `json:"status"`
	SubscriptionID   int64         `json:"subscription_id"`
	SubscriptionType string        `json:"subscription_type"`
	ExpiresAt        *string       `json:"expires_at,omitempty"`
	Accounts         []accountView `json:"accounts"`
}

type dashboardResponse struct {
	UserID     int64            `json:"user_id"`
	AllowReset bool             `json:"allow_reset"`
	Groups     []dashboardGroup `json:"groups"`
}

func (a *app) authenticateUser(request *http.Request) (int64, *requestError) {
	expectedUserID, err := strconv.ParseInt(strings.TrimSpace(request.Header.Get(embeddedUserIDHeader)), 10, 64)
	if err != nil || expectedUserID <= 0 {
		return 0, &requestError{Status: http.StatusUnauthorized, Code: 401, Message: "缺少有效的 Sub2API 用户 ID"}
	}
	token, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		return 0, &requestError{Status: http.StatusUnauthorized, Code: 401, Message: "缺少有效的 Sub2API 登录 Token"}
	}

	var user currentUser
	if upstreamErr := a.doUserRequest(request, http.MethodGet, "/auth/me", token, &user); upstreamErr != nil {
		if upstreamErr.Status == http.StatusUnauthorized || upstreamErr.Status == http.StatusForbidden {
			return 0, &requestError{Status: http.StatusUnauthorized, Code: 401, Message: "Sub2API 登录状态无效，请重新登录"}
		}
		return 0, &requestError{Status: http.StatusBadGateway, Code: 502, Message: "无法验证 Sub2API 登录状态"}
	}
	if user.ID <= 0 || user.ID != expectedUserID {
		return 0, &requestError{Status: http.StatusForbidden, Code: 403, Message: "用户 ID 与 Sub2API Token 不匹配"}
	}
	return user.ID, nil
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 8192 {
		return "", false
	}
	return parts[1], true
}

func (a *app) loadDashboard(ctx context.Context, userID int64, active bool) (*dashboardResponse, *requestError) {
	groups, requestErr := a.loadSubscribedGroups(ctx, userID)
	if requestErr != nil {
		return nil, requestErr
	}
	for index := range groups {
		accounts, upstreamErr := a.listGroupAccounts(ctx, groups[index].ID)
		if upstreamErr != nil {
			return nil, &requestError{Status: http.StatusBadGateway, Code: 502, Message: "无法读取订阅分组账号"}
		}
		groups[index].Accounts = accounts
	}
	a.loadAccountUsage(ctx, groups, active)
	return &dashboardResponse{UserID: userID, AllowReset: a.config.allowReset, Groups: groups}, nil
}

func (a *app) loadSubscribedGroups(ctx context.Context, userID int64) ([]dashboardGroup, *requestError) {
	var subscriptions []userSubscription
	path := "/admin/users/" + strconv.FormatInt(userID, 10) + "/subscriptions"
	if upstreamErr := a.doAdminRequest(ctx, http.MethodGet, path, nil, &subscriptions); upstreamErr != nil {
		return nil, &requestError{Status: http.StatusBadGateway, Code: 502, Message: "无法读取用户订阅"}
	}

	groups := make([]dashboardGroup, 0, len(subscriptions))
	seen := make(map[int64]struct{}, len(subscriptions))
	for _, subscription := range subscriptions {
		if subscription.Status != "active" || subscription.GroupID <= 0 {
			continue
		}
		if _, exists := seen[subscription.GroupID]; exists {
			continue
		}
		seen[subscription.GroupID] = struct{}{}
		group := dashboardGroup{
			ID:             subscription.GroupID,
			SubscriptionID: subscription.ID,
			ExpiresAt:      subscription.ExpiresAt,
			Accounts:       []accountView{},
		}
		if subscription.Group != nil {
			group.Name = subscription.Group.Name
			group.Platform = subscription.Group.Platform
			group.Status = subscription.Group.Status
			group.SubscriptionType = subscription.Group.SubscriptionType
		}
		groups = append(groups, group)
	}
	return groups, nil
}

func (a *app) listGroupAccounts(ctx context.Context, groupID int64) ([]accountView, *upstreamAPIError) {
	accounts := make([]accountView, 0)
	for page := 1; ; page++ {
		query := url.Values{
			"page":       {strconv.Itoa(page)},
			"page_size":  {strconv.Itoa(accountsPageSize)},
			"group":      {strconv.FormatInt(groupID, 10)},
			"sort_by":    {"id"},
			"sort_order": {"asc"},
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

func (a *app) loadAccountUsage(ctx context.Context, groups []dashboardGroup, active bool) {
	accountIDs := make(map[int64]struct{})
	for _, group := range groups {
		for _, account := range group.Accounts {
			accountIDs[account.ID] = struct{}{}
		}
	}

	type usageResult struct {
		data json.RawMessage
		err  string
	}
	results := make(map[int64]usageResult, len(accountIDs))
	var mutex sync.Mutex
	var waitGroup sync.WaitGroup
	semaphore := make(chan struct{}, usageConcurrency)
	for accountID := range accountIDs {
		accountID := accountID
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()

			query := url.Values{}
			if active {
				query.Set("source", "active")
				query.Set("force", "true")
			}
			var data json.RawMessage
			upstreamErr := a.doAdminRequest(ctx, http.MethodGet, "/admin/accounts/"+strconv.FormatInt(accountID, 10)+"/usage", query, &data)
			result := usageResult{data: data}
			if upstreamErr != nil {
				result.err = upstreamErr.Message
			}
			mutex.Lock()
			results[accountID] = result
			mutex.Unlock()
		}()
	}
	waitGroup.Wait()

	for groupIndex := range groups {
		for accountIndex := range groups[groupIndex].Accounts {
			result := results[groups[groupIndex].Accounts[accountIndex].ID]
			groups[groupIndex].Accounts[accountIndex].Usage = result.data
			groups[groupIndex].Accounts[accountIndex].UsageError = result.err
		}
	}
}

func (a *app) userCanAccessAccount(ctx context.Context, userID, accountID int64) (bool, *requestError) {
	groups, requestErr := a.loadSubscribedGroups(ctx, userID)
	if requestErr != nil {
		return false, requestErr
	}
	for _, group := range groups {
		accounts, upstreamErr := a.listGroupAccounts(ctx, group.ID)
		if upstreamErr != nil {
			return false, &requestError{Status: http.StatusBadGateway, Code: 502, Message: "无法校验账号所属分组"}
		}
		for _, account := range accounts {
			if account.ID == accountID {
				return true, nil
			}
		}
	}
	return false, nil
}

func (a *app) doUserRequest(source *http.Request, method, path, token string, out any) *upstreamAPIError {
	return a.doSub2APIRequest(source.Context(), method, path, nil, func(request *http.Request) {
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-User-UI-Request", "1")
		request.Header.Set("X-Admin-UI-Request", "1")
		request.Header.Set("User-Agent", source.UserAgent())
		if language := strings.TrimSpace(source.Header.Get("Accept-Language")); language != "" {
			request.Header.Set("Accept-Language", language)
		}
		if clientIP := requestClientIP(source, a.config.trustProxyHeaders); clientIP != "" {
			request.Header.Set("CF-Connecting-IP", clientIP)
			request.Header.Set("X-Real-IP", clientIP)
			request.Header.Set("X-Forwarded-For", clientIP)
		}
	}, out)
}

func (a *app) doAdminRequest(ctx context.Context, method, path string, query url.Values, out any) *upstreamAPIError {
	return a.doSub2APIRequest(ctx, method, path, query, func(request *http.Request) {
		request.Header.Set("x-api-key", a.config.adminAPIKey)
		request.Header.Set("X-Admin-UI-Request", "1")
		request.Header.Set("User-Agent", "sub2api-accountinfo/2.0")
	}, out)
}

func (a *app) doSub2APIRequest(ctx context.Context, method, path string, query url.Values, setHeaders func(*http.Request), out any) *upstreamAPIError {
	target := *a.config.sub2APIURL
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(path, "/")
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return &upstreamAPIError{Message: "无法创建 Sub2API 请求", Cause: err}
	}
	request.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	setHeaders(request)

	response, err := a.client.Do(request)
	if err != nil {
		return &upstreamAPIError{Message: "无法连接 Sub2API", Cause: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamBody+1))
	if err != nil {
		return &upstreamAPIError{Status: response.StatusCode, Message: "无法读取 Sub2API 响应", Cause: err}
	}
	if len(body) > maxUpstreamBody {
		return &upstreamAPIError{Status: response.StatusCode, Message: "Sub2API 响应过大"}
	}

	var envelope upstreamEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return &upstreamAPIError{Status: response.StatusCode, Message: "Sub2API 返回了无效响应", Cause: err}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !upstreamCodeSucceeded(envelope.Code) {
		message := strings.TrimSpace(envelope.Message)
		if message == "" {
			message = strings.TrimSpace(envelope.Reason)
		}
		if message == "" {
			message = fmt.Sprintf("Sub2API 请求失败（HTTP %d）", response.StatusCode)
		}
		apiErr := &upstreamAPIError{Status: response.StatusCode, Code: string(envelope.Code), Message: message}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			apiErr.Cause = errSub2APIUnauthorized
		}
		return apiErr
	}
	if out != nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return &upstreamAPIError{Status: response.StatusCode, Message: "无法解析 Sub2API 数据", Cause: err}
		}
	}
	return nil
}

func upstreamCodeSucceeded(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value == "" || value == "0" || value == "null"
}

func requestClientIP(request *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		for _, header := range []string{"CF-Connecting-IP", "X-Real-IP"} {
			if value := normalizedIP(request.Header.Get(header)); value != "" {
				return value
			}
		}
		for _, value := range strings.Split(request.Header.Get("X-Forwarded-For"), ",") {
			if value := normalizedIP(value); value != "" {
				return value
			}
		}
	}
	return normalizedIP(request.RemoteAddr)
}

func normalizedIP(value string) string {
	value = strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	if parsed := net.ParseIP(strings.Trim(value, "[]")); parsed != nil {
		return parsed.String()
	}
	return ""
}
