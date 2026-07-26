package main

import (
	"net/http"
	"sync"
	"time"
)

const accountResetDedupWindow = time.Minute

type accountResetState struct {
	mutex       sync.Mutex
	lastAttempt time.Time
}

type accountResetCoordinator struct {
	mutex      sync.Mutex
	identities map[string]*accountResetState
	now        func() time.Time
}

func newAccountResetCoordinator() *accountResetCoordinator {
	return &accountResetCoordinator{
		identities: make(map[string]*accountResetState),
		now:        time.Now,
	}
}

func (coordinator *accountResetCoordinator) withIdentity(
	identity string,
	action func(*accountResetState) *upstreamAPIError,
) *upstreamAPIError {
	coordinator.mutex.Lock()
	state, exists := coordinator.identities[identity]
	if !exists {
		state = &accountResetState{}
		coordinator.identities[identity] = state
	}
	coordinator.mutex.Unlock()

	state.mutex.Lock()
	defer state.mutex.Unlock()
	return action(state)
}

func (coordinator *accountResetCoordinator) beginAttempt(state *accountResetState) *upstreamAPIError {
	now := coordinator.now()
	if !state.lastAttempt.IsZero() && now.Sub(state.lastAttempt) < accountResetDedupWindow {
		return &upstreamAPIError{
			Status:  http.StatusConflict,
			Code:    "RESET_RECENTLY_ATTEMPTED",
			Message: "该账号刚刚执行过重置，请稍后再试",
		}
	}

	state.lastAttempt = now
	return nil
}

func (coordinator *accountResetCoordinator) finishAttempt(state *accountResetState) {
	state.lastAttempt = coordinator.now()
}
