package sub2api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
)

const defaultUserAgent = "Magpie Sub2API"

type tokenPair struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ExpiresIn int64  `json:"expires_in"`
	ExpiresAt int64  `json:"expires_at"`
	TwoFA     bool   `json:"requires_2fa"`
	Temporary string `json:"temp_token"`
}

func NormalizeBase(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", &Error{Kind: KindInvalid, Message: "Invalid Sub2API panel URL"}
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || ip != nil && ip.IsLoopback())) {
		return "", &Error{Kind: KindInvalid, Message: "Sub2API sign-in requires HTTPS"}
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	for _, suffix := range []string{"/api/v1", "/api", "/v1"} {
		if strings.HasSuffix(strings.ToLower(u.Path), suffix) {
			u.Path = strings.TrimRight(u.Path[:len(u.Path)-len(suffix)], "/")
			break
		}
	}
	u.RawPath = ""
	return u.String(), nil
}

func panelURL(base, raw string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", &Error{Kind: KindInvalid, Message: "Invalid panel URL"}
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", &Error{Kind: KindInvalid, Message: "Invalid panel request URL"}
	}
	if !u.IsAbs() {
		if u.Host != "" {
			return "", &Error{Kind: KindInvalid, Message: "Cross-origin panel request refused"}
		}
		u = b.ResolveReference(u)
	}
	if !sameOrigin(b, u) {
		return "", &Error{Kind: KindInvalid, Message: "Cross-origin panel request refused"}
	}
	return u.String(), nil
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func effectiveProxy(base, choice string) (string, error) {
	req, err := http.NewRequestWithContext(netproxy.With(context.Background(), choice), http.MethodGet, base, nil)
	if err != nil {
		return "", &Error{Kind: KindInvalid, Message: "Invalid panel network settings"}
	}
	proxy, err := netproxy.Func(req)
	if err != nil {
		return "", &Error{Kind: KindInvalid, Message: "Invalid panel proxy"}
	}
	if proxy == nil {
		return "direct", nil
	}
	return proxy.String(), nil
}

// CheckPanelHeaders keeps a source's other identity out of the panel session.
func CheckPanelHeaders(headers http.Header) error {
	for key := range headers {
		switch strings.ToLower(key) {
		case "cookie", "authorization", "proxy-authorization":
			return &Error{Kind: KindConflict, Message: "Remove identity headers before using the panel session"}
		}
	}
	return nil
}

func (m *Manager) request(ctx context.Context, s sessionSecret, method, raw string, headers http.Header, payload any, access string) ([]byte, error) {
	endpoint, err := panelURL(s.Base, raw)
	if err != nil {
		return nil, err
	}
	if err := CheckPanelHeaders(headers); err != nil {
		return nil, err
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, &Error{Kind: KindInvalid, Message: "Invalid panel request"}
		}
		body = bytes.NewReader(data)
	}
	ctx = netproxy.With(ctx, s.Proxy)
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, &Error{Kind: KindInvalid, Message: "Invalid panel request"}
	}
	req.Header = make(http.Header)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Accept", "application/json")
	ua := s.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	client := &http.Client{
		Transport: m.transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if via[0].Method != http.MethodGet && via[0].Method != http.MethodHead ||
				len(via) >= 5 || !sameOrigin(via[0].URL, next.URL) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &Error{Kind: KindTemporary, Message: "Sub2API panel request failed"}
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(rawBody) > 2<<20 {
		return nil, &Error{Kind: KindUnknown, Status: resp.StatusCode, Message: "Sub2API panel response could not be read"}
	}
	code := responseCode(rawBody)
	challenge := strings.EqualFold(resp.Header.Get("cf-mitigated"), "challenge")
	if challenge || resp.StatusCode < 200 || resp.StatusCode >= 300 || code != "" {
		e := &Error{Status: resp.StatusCode, Code: safeCode(code), RetryAfter: retryAfter(resp.Header.Get("Retry-After"), m.now()), Challenge: challenge, Kind: KindTemporary}
		switch {
		case challenge:
			e.Kind, e.Message = KindChallenge, "Complete verification on the original Sub2API site"
		case resp.StatusCode == 401 || code == "401":
			e.Kind, e.Message = KindAuth, "Sub2API credentials are no longer accepted"
		case resp.StatusCode == 429:
			e.Message = "Sub2API is rate limited"
		case resp.StatusCode == 403:
			e.Message = "Sub2API refused this request"
		default:
			e.Message = "Sub2API panel request was rejected"
		}
		return nil, e
	}
	return rawBody, nil
}

func parseTokens(raw []byte, now time.Time) (tokenPair, error) {
	var t tokenPair
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &t) != nil || json.Unmarshal(raw, &envelope) != nil {
		return t, &Error{Kind: KindUnknown, Message: "Invalid Sub2API sign-in response"}
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		var nested tokenPair
		if json.Unmarshal(envelope.Data, &nested) != nil {
			return t, &Error{Kind: KindUnknown, Message: "Invalid Sub2API sign-in response"}
		}
		if nested.Access != "" {
			t.Access = nested.Access
		}
		if nested.Refresh != "" {
			t.Refresh = nested.Refresh
		}
		if nested.ExpiresIn > 0 {
			t.ExpiresIn = nested.ExpiresIn
		}
		if nested.ExpiresAt > 0 {
			t.ExpiresAt = nested.ExpiresAt
		}
		t.TwoFA, t.Temporary = nested.TwoFA, nested.Temporary
	}
	if t.TwoFA && t.Temporary != "" {
		return t, nil
	}
	if strings.TrimSpace(t.Access) == "" || strings.TrimSpace(t.Refresh) == "" {
		return t, &Error{Kind: KindUnknown, Message: "Sub2API did not return a complete token pair"}
	}
	if t.ExpiresAt <= 0 && t.ExpiresIn > 0 {
		t.ExpiresAt = now.Add(time.Duration(t.ExpiresIn) * time.Second).Unix()
	}
	return t, nil
}

func responseCode(raw []byte) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return ""
	}
	if code := strings.TrimSpace(string(object["code"])); code != "" && code != "null" {
		var text string
		if json.Unmarshal(object["code"], &text) == nil {
			code = text
		}
		if code != "" && code != "0" && code != "200" {
			return code
		}
	}
	for _, key := range []string{"error", "detail", "data"} {
		if code := responseCode(object[key]); code != "" {
			return code
		}
	}
	return ""
}

func safeCode(code string) string {
	if len(code) > 100 {
		return ""
	}
	for _, r := range code {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return ""
		}
	}
	return code
}

func retryAfter(raw string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func accessRejected(err error) bool {
	var e *Error
	return errors.As(err, &e) && !e.Challenge && (e.Status == 401 || e.Code == "401")
}

func refreshRejected(err error) bool {
	if accessRejected(err) {
		return true
	}
	var e *Error
	if !errors.As(err, &e) || e.Challenge {
		return false
	}
	switch strings.ToUpper(e.Code) {
	case "REFRESH_TOKEN_INVALID", "REFRESH_TOKEN_EXPIRED", "REFRESH_TOKEN_REUSED", "INVALID_REFRESH_TOKEN", "TOKEN_REVOKED", "REFRESH_TOKEN_REVOKED", "SESSION_BINDING_MISMATCH":
		return true
	}
	return false
}
