package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testAppOrigin = "https://app.example"

// newTestServer 允许回环地址，使 httptest 上游可以作为白名单目标。
func newTestServer(t *testing.T, targets ...string) *proxyServer {
	t.Helper()
	policy, err := newProxyPolicy(strings.Join(targets, ","), testAppOrigin)
	if err != nil {
		t.Fatal(err)
	}
	policy.allowLoopback = true
	return newProxyServer(policy)
}

func countingUpstream(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(upstream.Close)
	return upstream, hits
}

func proxyRequest(t *testing.T, server *proxyServer, envelope proxyEnvelope, origin string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/__proxy__/request", bytes.NewReader(payload))
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	result := httptest.NewRecorder()
	server.handleRequest(result, request)
	return result
}

func TestDefaultDenyRejectsEveryTarget(t *testing.T) {
	upstream, hits := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	policy, err := newProxyPolicy("", "")
	if err != nil {
		t.Fatal(err)
	}
	policy.allowLoopback = true
	server := newProxyServer(policy)

	result := proxyRequest(t, server, proxyEnvelope{Method: "GET", URL: upstream.URL}, "")
	if result.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", result.Code)
	}
	if hits.Load() != 0 {
		t.Fatal("upstream was reached without an allowlist")
	}

	health := httptest.NewRecorder()
	server.handleHealth(health, httptest.NewRequest(http.MethodGet, "/__proxy__/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", health.Code)
	}
}

func TestPolicyRejectsInvalidOrigins(t *testing.T) {
	for _, raw := range []string{"https://*.njust.edu.cn", "ftp://bkjw.njust.edu.cn", "https://bkjw.njust.edu.cn/path", "https://user@bkjw.njust.edu.cn", "*.njust.edu.cn/path"} {
		if _, err := newProxyPolicy(raw, ""); err == nil {
			t.Errorf("newProxyPolicy(%q) accepted an invalid target", raw)
		}
	}

	policy, err := newProxyPolicy("https://bkjw.njust.edu.cn", "")
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]bool{
		"https://bkjw.njust.edu.cn/njlgdx/x": true,
		"https://BKJW.njust.edu.cn:443/":     true,
		"http://bkjw.njust.edu.cn/":          false,
		"https://bkjw.njust.edu.cn:8443/":    false,
		"https://evil.njust.edu.cn/":         false,
		"https://bkjw.njust.edu.cn.evil/":    false,
	} {
		u, _ := url.Parse(raw)
		if got := policy.checkTarget(u) == nil; got != want {
			t.Errorf("checkTarget(%q) allowed=%t, want %t", raw, got, want)
		}
	}
}

func TestLoopbackTargetRejectedAtConnectTime(t *testing.T) {
	upstream, hits := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "SYNTHETIC_INTERNAL_SENTINEL")
	})
	// 目标在白名单里，但没有测试豁免：连接时必须因回环地址被拒。
	policy, err := newProxyPolicy(upstream.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	server := newProxyServer(policy)

	result := proxyRequest(t, server, proxyEnvelope{Method: "GET", URL: upstream.URL}, "")
	if result.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", result.Code)
	}
	if hits.Load() != 0 {
		t.Fatal("loopback upstream was reached")
	}

	// 域名解析到回环同样要拒绝（模拟白名单域名被重绑定到内网）。
	upstreamURL, _ := url.Parse(upstream.URL)
	if _, err := policy.dialContext(context.Background(), "tcp", "localhost:"+upstreamURL.Port()); !errors.Is(err, errTargetNotAllowed) {
		t.Fatalf("dial localhost err = %v, want errTargetNotAllowed", err)
	}
}

func TestIsPublicAddr(t *testing.T) {
	policy := &proxyPolicy{}
	blocked := []string{
		"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "fc00::1",
		"169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1",
		"100.64.0.1", "::ffff:8.8.8.8", "::ffff:127.0.0.1",
	}
	for _, raw := range blocked {
		if policy.isPublicAddr(netip.MustParseAddr(raw)) {
			t.Errorf("%s should be blocked", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "2001:4860:4860::8888", "202.119.81.1"} {
		if !policy.isPublicAddr(netip.MustParseAddr(raw)) {
			t.Errorf("%s should be allowed", raw)
		}
	}
}

func TestCORSOrigins(t *testing.T) {
	server := newTestServer(t)

	for _, method := range []string{http.MethodOptions, http.MethodPost} {
		request := httptest.NewRequest(method, "/__proxy__/request", strings.NewReader("{}"))
		request.Header.Set("Origin", "https://untrusted.example")
		result := httptest.NewRecorder()
		server.handleRequest(result, request)
		if result.Code != http.StatusForbidden {
			t.Errorf("%s untrusted origin status = %d, want 403", method, result.Code)
		}
		if got := result.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s untrusted origin got Access-Control-Allow-Origin %q", method, got)
		}
	}

	request := httptest.NewRequest(http.MethodOptions, "/__proxy__/session", nil)
	request.Header.Set("Origin", testAppOrigin)
	result := httptest.NewRecorder()
	server.handleSession(result, request)
	if result.Code != http.StatusNoContent {
		t.Fatalf("trusted preflight status = %d, want 204", result.Code)
	}
	header := result.Header()
	if header.Get("Access-Control-Allow-Origin") != testAppOrigin ||
		header.Get("Access-Control-Allow-Credentials") != "true" ||
		header.Get("Vary") != "Origin" {
		t.Fatalf("trusted preflight headers = %v", header)
	}

	result = httptest.NewRecorder()
	server.handleSession(result, httptest.NewRequest(http.MethodPost, "/__proxy__/session", nil))
	if result.Code != http.StatusOK || result.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("no-origin request status = %d headers = %v", result.Code, result.Header())
	}
}

func TestAllowedRequestSucceeds(t *testing.T) {
	upstream, _ := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	})
	server := newTestServer(t, upstream.URL)

	result := proxyRequest(t, server, proxyEnvelope{Method: "GET", URL: upstream.URL + "/page"}, testAppOrigin)
	if result.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", result.Code, result.Body.String())
	}
	var body proxyResponse
	if err := json.Unmarshal(result.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	content, _ := base64.StdEncoding.DecodeString(body.BodyBase64)
	if body.StatusCode != http.StatusOK || string(content) != "hello" || body.Headers["content-type"] != "text/plain" {
		t.Fatalf("unexpected response %+v content=%q", body, content)
	}
}

func TestRedirectToForbiddenHostRejected(t *testing.T) {
	forbidden, forbiddenHits := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	allowed, _ := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, forbidden.URL+"/secret", http.StatusFound)
	})
	server := newTestServer(t, allowed.URL)

	result := proxyRequest(t, server, proxyEnvelope{Method: "GET", URL: allowed.URL, FollowRedirects: true}, "")
	if result.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", result.Code)
	}
	if forbiddenHits.Load() != 0 {
		t.Fatal("redirect reached a host outside the allowlist")
	}

	// 不跟随跳转时把 302 原样交回客户端，由客户端再发起（并再次校验）下一跳。
	result = proxyRequest(t, server, proxyEnvelope{Method: "GET", URL: allowed.URL}, "")
	var body proxyResponse
	json.Unmarshal(result.Body.Bytes(), &body)
	if body.StatusCode != http.StatusFound || body.Headers["location"] != forbidden.URL+"/secret" {
		t.Fatalf("unexpected non-follow response %+v", body)
	}
}

func TestRedirectAcrossOriginsDropsClientCredentials(t *testing.T) {
	var seen sync.Map
	other, _ := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Store("cookie", r.Header.Get("Cookie"))
		seen.Store("authorization", r.Header.Get("Authorization"))
	})
	start, _ := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/same", http.StatusFound)
			return
		}
		seen.Store("same-authorization", r.Header.Get("Authorization"))
		http.Redirect(w, r, other.URL+"/next", http.StatusFound)
	})
	server := newTestServer(t, start.URL, other.URL)

	result := proxyRequest(t, server, proxyEnvelope{
		Method:          "GET",
		URL:             start.URL + "/",
		Headers:         map[string]string{"Cookie": "client=1", "Authorization": "Bearer client"},
		FollowRedirects: true,
	}, "")
	if result.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", result.Code, result.Body.String())
	}
	if got, _ := seen.Load("same-authorization"); got != "Bearer client" {
		t.Errorf("same-origin redirect authorization = %q, want kept", got)
	}
	if got, _ := seen.Load("cookie"); got != "" {
		t.Errorf("cross-origin redirect cookie = %q, want empty", got)
	}
	if got, _ := seen.Load("authorization"); got != "" {
		t.Errorf("cross-origin redirect authorization = %q, want empty", got)
	}
}

func TestRedirectLimit(t *testing.T) {
	upstream, hits := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	server := newTestServer(t, upstream.URL)

	result := proxyRequest(t, server, proxyEnvelope{Method: "GET", URL: upstream.URL, FollowRedirects: true}, "")
	if result.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", result.Code)
	}
	if got := hits.Load(); got != maxRedirects+1 {
		t.Fatalf("upstream hits = %d, want %d", got, maxRedirects+1)
	}
}

func TestHasActiveLoginFollowsCookieJar(t *testing.T) {
	upstream, _ := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/njlgdx/login":
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "synthetic", Path: "/njlgdx"})
		case "/njlgdx/logout":
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "", Path: "/njlgdx", MaxAge: -1})
		case "/other/empty":
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "", Path: "/"})
		}
	})
	server := newTestServer(t, upstream.URL)
	session := server.newProxySession()
	send := func(path string) {
		t.Helper()
		target, _ := url.Parse(upstream.URL + path)
		if _, err := session.send("GET", target, nil, nil, false); err != nil {
			t.Fatal(err)
		}
	}

	send("/other/empty")
	if session.hasActiveLogin() {
		t.Fatal("empty JSESSIONID counted as login")
	}
	send("/njlgdx/login")
	if !session.hasActiveLogin() {
		t.Fatal("path-scoped JSESSIONID not detected")
	}
	send("/njlgdx/logout")
	if session.hasActiveLogin() {
		t.Fatal("expired JSESSIONID still counted as login")
	}
}

func TestConcurrentSendAndHasActiveLogin(t *testing.T) {
	upstream, _ := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "synthetic-only", Path: "/"})
		io.WriteString(w, "ok")
	})
	server := newTestServer(t, upstream.URL)
	session := server.newProxySession()

	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			for j := 0; j < 12; j++ {
				target, _ := url.Parse(upstream.URL + "/p" + string(rune('a'+i)) + "/x")
				if _, err := session.send("GET", target, nil, nil, j%2 == 0); err != nil {
					t.Error(err)
				}
				session.hasActiveLogin()
			}
		}(i)
	}
	group.Wait()
	if !session.hasActiveLogin() {
		t.Fatal("JSESSIONID not detected after concurrent requests")
	}
}

// 会话 Cookie 必须带与 TTL 一致的 MaxAge，否则浏览器关掉就丢，
// 服务端 TTL 再长也接不上同一个会话。
func TestSessionCookieCarriesTTL(t *testing.T) {
	policy, err := newProxyPolicy("", testAppOrigin)
	if err != nil {
		t.Fatal(err)
	}
	const ttl = 12 * time.Hour
	server := newProxyServerWithTTL(policy, ttl)
	// 浏览器那枚 Cookie 跟凭据寿命走，不是内存 TTL；这里显式对齐后断言。
	server.credentialTTL = ttl

	request := httptest.NewRequest(http.MethodPost, "/__proxy__/session", strings.NewReader("{}"))
	request.Header.Set("Origin", testAppOrigin)
	result := httptest.NewRecorder()
	server.handleSession(result, request)

	cookies := result.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected one session cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != proxySessionCookieName {
		t.Fatalf("unexpected cookie name %q", cookie.Name)
	}
	if cookie.MaxAge != int(ttl.Seconds()) {
		t.Fatalf("MaxAge = %d, want %d", cookie.MaxAge, int(ttl.Seconds()))
	}
	if cookie.Expires.IsZero() {
		t.Fatal("Expires must be set together with MaxAge")
	}
	// 测试请求是 http，没有 TLS，因此退化为 Lax，避免浏览器丢弃 None+非 Secure 的 Cookie。
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax on plain http", cookie.SameSite)
	}
}

// 通配后缀与 CIDR 是新增的白名单写法：后缀放行任意子域，IP 段放行裸 IP 目标。
func TestPolicyWildcardSuffixAndCIDR(t *testing.T) {
	policy, err := newProxyPolicy("*.njust.edu.cn,202.119.0.0/16", "")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"https://bkjw.njust.edu.cn/njlgdx/x":   true,
		"https://ehall2.njust.edu.cn:443/login": true,
		"http://ehall.njust.edu.cn/":            true,
		"http://202.119.81.112:9080/x":          true,
		"https://202.119.83.29/":                true,
		"https://njust.edu.cn/":                 true,
		"https://evil.example.com/":             false,
		"https://njust.edu.cn.evil.com/":        false,
		"http://10.0.0.1/":                      false,
		"http://202.119.81.112:9999/":           false,
		"http://bkjw.njust.edu.cn:8081/":        false,
	}
	for raw, want := range cases {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got := policy.checkTarget(target) == nil; got != want {
			t.Errorf("checkTarget(%q) = %v, want %v", raw, got, want)
		}
	}
}

// 后台展示的学号只从 CAS 登录表单里取，别的请求体一律不碰。
func TestLoginAccountFromBody(t *testing.T) {
	loginURL, _ := url.Parse("https://ids.njust.edu.cn/authserver/login?service=x")
	body := []byte("username=922110800724&password=ENCRYPTED&_eventId=submit")
	if got := loginAccountFromBody("POST", loginURL, body); got != "922110800724" {
		t.Fatalf("account = %q", got)
	}
	if got := loginAccountFromBody("GET", loginURL, body); got != "" {
		t.Fatalf("GET must not be parsed, got %q", got)
	}
	otherURL, _ := url.Parse("https://bkjw.njust.edu.cn/njlgdx/xskb/xskb_list.do")
	if got := loginAccountFromBody("POST", otherURL, body); got != "" {
		t.Fatalf("non-ids target must not be parsed, got %q", got)
	}
	if got := loginAccountFromBody("POST", loginURL, []byte("not-a-form")); got != "" {
		t.Fatalf("garbage body should yield empty, got %q", got)
	}
}

// 默认不信任 X-Forwarded-For，避免客户端伪造来源。
func TestClientIPIgnoresForwardedByDefault(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/__proxy__/session", nil)
	request.RemoteAddr = "203.0.113.9:5555"
	request.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(request, false); got != "203.0.113.9" {
		t.Fatalf("clientIP = %q, want the socket address", got)
	}
	if got := clientIP(request, true); got != "1.2.3.4" {
		t.Fatalf("clientIP with trust = %q, want forwarded address", got)
	}
}
