package sub2api

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// BFF reads can rotate a one-time refresh token, so they share renewal's ownership.
func (m *Manager) lockCookieAuth(ctx context.Context, base string) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		unlock, err := m.lockAuth(base)
		if err == nil {
			return unlock, nil
		}
		var detail *Error
		if !errors.As(err, &detail) || detail.Code != "AUTH_BUSY" {
			return nil, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) readCookie(ctx context.Context, id, method, rawURL string, headers http.Header, renew bool) ([]byte, error) {
	initial, err := m.record(id)
	if err != nil {
		return nil, err
	}
	unlockAuth, err := m.lockCookieAuth(ctx, initial.View.Base)
	if err != nil {
		return nil, err
	}
	defer unlockAuth()
	unlock, err := m.lockAccount(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	record, secret, err := m.load(id)
	if err != nil {
		return nil, err
	}
	if secret.Tokens.SessionCookie == "" {
		return nil, staleSession()
	}
	if record.View.State == StateUnknown {
		return nil, &Error{Kind: KindUnknown, Code: "REFRESH_UNCERTAIN", Message: "The previous refresh result is uncertain; sign in again"}
	}
	if record.View.State == StateLogin {
		if !renew || !record.View.Remember || record.AutoDone == secret.Version {
			return nil, &Error{Kind: KindAuth, Message: "Sign in to this Sub2API account again"}
		}
		if err := m.reloginLocked(ctx, &record, secret); err != nil {
			return nil, err
		}
		if method == http.MethodPost {
			return nil, &Error{Kind: KindRenewed, Message: "Sub2API session renewed; query again"}
		}
		record, secret, err = m.load(id)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := panelURL(secret.Base, rawURL); err != nil {
		return nil, err
	}
	if err := CheckPanelHeaders(headers); err != nil {
		return nil, err
	}
	attempt, err := randomID()
	if err != nil {
		return nil, err
	}
	before := record
	record.Pending = &pendingRotation{Version: secret.Version, Generation: secret.Generation,
		Hash: refreshFingerprint(secret.Tokens.SessionCookie), Attempt: attempt}
	if err := m.commitRecord(before, record); err != nil {
		return nil, err
	}
	// Once accepted, a silent rotation must finish even if its caller stops waiting.
	owner, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	old := secret.Tokens
	raw, readErr := m.requestSession(owner, &secret, method, rawURL, headers, nil, "")
	if secret.Tokens != old {
		secret.Version, secret.Attempt = secret.Version+1, attempt
		record.View.Version = secret.Version
		record.View.UpdatedAt = m.now()
		if readErr == nil {
			record.View.State, record.View.LastError, record.View.LastSuccess = StateReady, "", m.now()
		}
		record.Pending = nil
		if err := m.persist(record, secret); err != nil {
			return nil, err
		}
	} else if readErr != nil {
		var detail *Error
		if errors.As(readErr, &detail) && detail.Kind == KindAuth {
			return nil, m.saveFailure(&record, StateLogin, readErr, false)
		}
		if detail == nil || !detail.notSent && (detail.Status == 0 || detail.Status >= 500 || detail.Kind == KindUnknown) {
			return nil, m.saveFailure(&record, StateUnknown, readErr, true)
		}
		before = record
		record.Pending = nil
		if err := m.commitRecord(before, record); err != nil {
			return nil, err
		}
	} else {
		before = record
		record.Pending = nil
		if err := m.commitRecord(before, record); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, readErr
}
