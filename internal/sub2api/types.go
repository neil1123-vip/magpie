// Package sub2api owns local panel sessions without exposing their credentials.
package sub2api

import (
	"time"
)

const (
	StateReady     = "ready"
	StateTwoFA     = "two_factor"
	StateChallenge = "challenge"
	StateLogin     = "login_required"
	StateTemporary = "temporary"
	StateUnknown   = "refresh_unknown"
	StateLoggedOut = "logged_out"
)

const (
	KindAuth      = "auth"
	KindTemporary = "temporary"
	KindChallenge = "challenge"
	KindInvalid   = "invalid"
	KindUnknown   = "unknown"
	KindStorage   = "storage"
	KindConflict  = "conflict"
	KindRenewed   = "renewed"
)

// Error retains classification while keeping vendor bodies and secrets private.
type Error struct {
	Kind       string        `json:"kind"`
	Status     int           `json:"status,omitempty"`
	Code       string        `json:"code,omitempty"`
	RetryAfter time.Duration `json:"retryAfter,omitempty"`
	Challenge  bool          `json:"challenge,omitempty"`
	Message    string        `json:"message"`
}

func (e *Error) Error() string { return e.Message }

type LoginRequest struct {
	browser            bool
	Base               string `json:"base"`
	Email              string `json:"email"`
	Password           string `json:"password"`
	Remember           bool   `json:"remember"`
	Proxy              string `json:"proxy,omitempty"`
	UserAgent          string `json:"userAgent,omitempty"`
	Target             string `json:"target"`
	Revision           string `json:"revision"`
	SessionID          string `json:"sessionId,omitempty"`
	ExpectedGeneration uint64 `json:"expectedGeneration,omitempty"`
}

type LoginResult struct {
	State     string       `json:"state"`
	Attempt   string       `json:"attempt,omitempty"`
	Target    string       `json:"target,omitempty"`
	Revision  string       `json:"revision,omitempty"`
	SessionID string       `json:"sessionId,omitempty"`
	Account   *AccountView `json:"account,omitempty"`
}

// AccountView is safe for the GUI; Proxy is redacted, never a request setting.
type AccountView struct {
	ID             string    `json:"id"`
	Base           string    `json:"base"`
	Subject        string    `json:"subject"`
	Email          string    `json:"email"`
	State          string    `json:"state"`
	Version        uint64    `json:"version"`
	Generation     uint64    `json:"generation"`
	UpdatedAt      time.Time `json:"updatedAt"`
	LastSuccess    time.Time `json:"lastSuccess,omitempty"`
	LastError      string    `json:"lastError,omitempty"`
	RetryAt        time.Time `json:"retryAt,omitempty"`
	Remember       bool      `json:"remember"`
	Proxy          string    `json:"proxy,omitempty"`
	UserAgent      string    `json:"userAgent,omitempty"`
	Context        string    `json:"context,omitempty"`
	CleanupPending bool      `json:"cleanupPending,omitempty"`
}

type LogoutResult struct {
	Generation     uint64 `json:"generation"`
	CleanupPending bool   `json:"cleanupPending"`
	CleanupError   string `json:"cleanupError,omitempty"`
}
