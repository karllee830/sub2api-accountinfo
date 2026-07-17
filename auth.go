package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type sub2APIAuthState struct {
	mutex        sync.Mutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
}

type sub2APIAuthEnvelope struct {
	Code    int                      `json:"code"`
	Message string                   `json:"message"`
	Reason  string                   `json:"reason"`
	Data    sub2APIAuthTokenResponse `json:"data"`
}

type sub2APIAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Requires2FA  bool   `json:"requires_2fa"`
}

func (a *app) usesPasswordAuthentication() bool {
	return a.config.sub2APIAdminEmail != ""
}

func (a *app) getSub2APIAccessToken(ctx context.Context) (string, error) {
	a.auth.mutex.Lock()
	defer a.auth.mutex.Unlock()

	if !a.usesPasswordAuthentication() {
		if a.auth.accessToken == "" {
			return "", errors.New("static access token is empty")
		}
		return a.auth.accessToken, nil
	}

	if a.auth.accessToken != "" && (a.auth.expiresAt.IsZero() || time.Until(a.auth.expiresAt) > 5*time.Second) {
		return a.auth.accessToken, nil
	}
	return a.refreshOrLoginSub2APILocked(ctx)
}

func (a *app) renewSub2APIAccessToken(ctx context.Context, rejectedToken string) (string, error) {
	a.auth.mutex.Lock()
	defer a.auth.mutex.Unlock()

	if a.auth.accessToken != "" && a.auth.accessToken != rejectedToken {
		return a.auth.accessToken, nil
	}
	return a.refreshOrLoginSub2APILocked(ctx)
}

func (a *app) refreshOrLoginSub2APILocked(ctx context.Context) (string, error) {
	if a.auth.refreshToken != "" {
		response, err := a.requestSub2APIAuthToken(ctx, "/auth/refresh", map[string]string{
			"refresh_token": a.auth.refreshToken,
		})
		if err == nil {
			a.storeSub2APIAuthTokenLocked(response)
			return a.auth.accessToken, nil
		}
		a.auth.refreshToken = ""
	}

	response, err := a.requestSub2APIAuthToken(ctx, "/auth/login", map[string]string{
		"email":    a.config.sub2APIAdminEmail,
		"password": a.config.sub2APIAdminPassword,
	})
	if err != nil {
		return "", err
	}
	if response.Requires2FA {
		return "", errors.New("administrator account requires TOTP 2FA; use SUB2API_AUTH_TOKEN or disable 2FA for this service account")
	}
	a.storeSub2APIAuthTokenLocked(response)
	return a.auth.accessToken, nil
}

func (a *app) requestSub2APIAuthToken(ctx context.Context, path string, payload map[string]string) (sub2APIAuthTokenResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return sub2APIAuthTokenResponse{}, fmt.Errorf("encode authentication request: %w", err)
	}
	target := *a.config.sub2APIURL
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawQuery = ""

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return sub2APIAuthTokenResponse{}, fmt.Errorf("create authentication request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "sub2api-accountinfo/1.0")

	response, err := a.client.Do(request)
	if err != nil {
		return sub2APIAuthTokenResponse{}, fmt.Errorf("request authentication token: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamBody+1))
	if err != nil {
		return sub2APIAuthTokenResponse{}, fmt.Errorf("read authentication response: %w", err)
	}
	if len(responseBody) > maxUpstreamBody {
		return sub2APIAuthTokenResponse{}, errors.New("authentication response is too large")
	}

	var envelope sub2APIAuthEnvelope
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return sub2APIAuthTokenResponse{}, fmt.Errorf("invalid authentication response (HTTP %d)", response.StatusCode)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || envelope.Code != 0 {
		message := strings.TrimSpace(envelope.Message)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", response.StatusCode)
		}
		if envelope.Reason != "" {
			message += " (" + envelope.Reason + ")"
		}
		return sub2APIAuthTokenResponse{}, errors.New(message)
	}
	if !envelope.Data.Requires2FA && strings.TrimSpace(envelope.Data.AccessToken) == "" {
		return sub2APIAuthTokenResponse{}, errors.New("authentication response did not contain an access token")
	}
	return envelope.Data, nil
}

func (a *app) storeSub2APIAuthTokenLocked(response sub2APIAuthTokenResponse) {
	a.auth.accessToken = strings.TrimSpace(response.AccessToken)
	a.auth.refreshToken = strings.TrimSpace(response.RefreshToken)
	if response.ExpiresIn > 0 {
		a.auth.expiresAt = time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	} else {
		a.auth.expiresAt = time.Time{}
	}
}

func (a *app) doSub2APIRequest(ctx context.Context, method, target, authToken string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Language", "zh-CN")
	request.Header.Set("Authorization", "Bearer "+authToken)
	request.Header.Set("User-Agent", "sub2api-accountinfo/1.0")
	request.Header.Set("X-Admin-UI-Request", "1")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	return a.client.Do(request)
}
