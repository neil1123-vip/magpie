package sub2apibrowser

import (
	"strings"
	"testing"
)

func TestCandidateTrustBoundary(t *testing.T) {
	good := `{"origin":"https://panel.test:443","access":"access","refresh":"refresh","userAgent":"fixture"}`
	if _, err := decodeCandidate([]byte(good), "https://panel.test/path"); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		strings.Replace(good, "panel.test", "other.test", 1),
		strings.Replace(good, `"refresh"`, `""`, 1),
		strings.Replace(good, "fixture", `fixture\ninjected`, 1),
		good + `{}`,
		good + `garbage`,
		strings.Replace(good, `"access":"access"`, `"unknown":true,"access":"access"`, 1),
	} {
		if _, err := decodeCandidate([]byte(data), "https://panel.test"); err == nil {
			t.Errorf("accepted invalid candidate: %s", data)
		}
	}
}

func TestNativeRequestPolicy(t *testing.T) {
	for _, source := range []uint32{1, 2, 4} {
		for _, path := range []string{"/api/v1/auth/refresh", "/api/v1/auth/refresh/", "/api/v1/auth/x/../refresh"} {
			if !blockedRequest("https://panel.test"+path, "https://panel.test", source, 5) {
				t.Errorf("refresh %s allowed from source %d", path, source)
			}
		}
	}
	for _, c := range []struct {
		url              string
		source, resource uint32
		blocked          bool
	}{
		{"https://panel.test/api/v1/auth/me", 1, 5, false},
		{"https://elsewhere.test/login", 1, 1, true},
		{"https://challenges.cloudflare.com/turnstile/frame", 1, 1, false},
		{"https://challenges.cloudflare.com.evil.test/frame", 1, 1, true},
		{"https://cdn.test/script.js", 1, 3, false},
		{"https://cdn.test/service.js", 4, 3, true},
		{"http://cdn.test/script.js", 1, 3, true},
	} {
		if got := blockedRequest(c.url, "https://panel.test", c.source, c.resource); got != c.blocked {
			t.Errorf("%s source %d resource %d blocked=%v", c.url, c.source, c.resource, got)
		}
	}
}
