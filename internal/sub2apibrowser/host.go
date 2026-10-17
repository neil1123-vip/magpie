// Package sub2apibrowser runs the short-lived, isolated panel sign-in window.
package sub2apibrowser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"strings"
)

const hostArg = "--sub2api-browser-host"
const unavailable = "Browser verification is unavailable in this build."
const maxMessage = 64 << 10

type wireRequest struct {
	Base      string `json:"base"`
	Proxy     string `json:"proxy"`
	UserAgent string `json:"userAgent"`
	Profile   string `json:"profile"`
}

type wireCandidate struct {
	Origin    string `json:"origin"`
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	UserAgent string `json:"userAgent"`
}

func decodeCandidate(data []byte, base string) (wireCandidate, error) {
	var c wireCandidate
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if len(data) > maxMessage || decoder.Decode(&c) != nil ||
		!sameOrigin(c.Origin, base) || c.Access == "" || c.Refresh == "" ||
		len(c.Access) > 24000 || len(c.Refresh) > 24000 || len(c.UserAgent) > 2048 ||
		strings.ContainsAny(c.Access+c.Refresh+c.UserAgent, "\r\n\x00") {
		return wireCandidate{}, fmt.Errorf("Invalid browser sign-in result")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return wireCandidate{}, fmt.Errorf("Invalid browser sign-in result")
	}
	return c, nil
}

func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Hostname() == "" ||
		(u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func sameOrigin(a, b string) bool {
	return origin(a) != "" && origin(a) == origin(b)
}

func blockedRequest(raw, base string, source, resource uint32) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	p := strings.TrimRight(strings.ToLower(path.Clean(u.Path)), "/")
	if strings.HasSuffix(p, "/auth/refresh") {
		return true
	}
	if sameOrigin(raw, base) {
		return false
	}
	// Only Cloudflare challenge frames may load another origin's document.
	if resource == 1 {
		return u.Scheme != "https" || u.Host != "challenges.cloudflare.com"
	}
	return source != 1 || u.Scheme != "https"
}

func captureScript(base string) string {
	expected, _ := json.Marshal(base)
	return `(function(){
if(window.top!==window)return;
const base=new URL(` + string(expected) + `);
if(location.origin!==base.origin)return;
let sent=false;
function capture(){
 if(sent)return;
 try{
  const access=localStorage.getItem('auth_token'),refresh=localStorage.getItem('refresh_token');
  if(!access||!refresh)return;
  sent=true;
  chrome.webview.postMessage('sub2api-candidate:'+JSON.stringify({
   origin:location.origin,access,refresh,userAgent:navigator.userAgent
  }));
 }catch(_){}
}
setInterval(capture,250);
addEventListener('storage',capture);
addEventListener('DOMContentLoaded',capture);
})();`
}
