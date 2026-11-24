package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	proxySessionCookieName = "njust_schedule_proxy_session"
	// 默认 12 小时：覆盖「当天离开、隔夜回来」的典型间隔。
	// 原来是 2 小时，客户的浏览器会话基本撑不到第二天，每次都要重走完整登录。
	defaultSessionTTL = 12 * time.Hour
	cleanupInterval   = 10 * time.Minute
	checkpointInterval = 20 * time.Second
	maxRedirects      = 10
	// 通配域名与 CIDR 目标只允许这些端口；精确 origin 不受此限制。
	// 9080 是校内个别服务的端口（见 docs/Web代理.md 里的 202.119.81.112:9080）。
	defaultAllowedTargetPorts = "80,443,8080,9080"
	// 浏览器会话 Cookie 与凭据库的寿命。CAS 的 CASTGC 通常 30 天，
	// 这里取 45 天留出余量：上游先过期就自然是重登，不会因为本地先删而浪费。
	defaultCredentialTTL = 45 * 24 * time.Hour
	defaultMaxSessions   = 5000
	defaultMaxStoreEntries = 20000
)

var (
	passthroughHeaders = map[string]struct{}{
		"content-type":  {},
		"location":      {},
		"cache-control": {},
	}

	errTargetNotAllowed = errors.New("proxy target not allowed")

	// 公网之外但 netip 没有现成判断的网段：CGNAT 与 0.0.0.0/8。
	blockedPrefixes = []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("0.0.0.0/8"),
	}
)

// proxyPolicy 决定代理可以访问哪些目标、接受哪些网页来源。
type proxyPolicy struct {
	allowedTargets map[string]struct{}
	targetURLs     []*url.URL
	allowedOrigins map[string]struct{}
	// allowedSuffixes 形如 ".njust.edu.cn"：任意子域的 http/https 都放行，
	// 端口仍需命中 allowedPorts。允许整段校园域名，省得每加一个二级站就改一次配置。
	allowedSuffixes []string
	// allowedCIDRs 用于裸 IP 目标（如 202.119.81.112:9080）。IP 目标不走后缀匹配，
	// 必须在连接时按解析结果校验，否则等于开放代理。
	allowedCIDRs []netip.Prefix
	// allowedPorts 只约束后缀与 CIDR 命中的目标；精确 origin 用自己写的端口。
	allowedPorts map[string]struct{}
	// 仅测试使用：允许连接回环地址，以便用 httptest 起上游。
	allowLoopback bool
}

type proxyServer struct {
	policy    *proxyPolicy
	transport http.RoundTripper
	// sessionTTL 是内存会话的空闲上限：超时就清掉，控制占用。
	sessionTTL time.Duration
	// credentialTTL 是下发给浏览器的会话 Cookie 有效期，也是凭据库的保底寿命。
	// 它比 sessionTTL 长：内存里没了可以从凭据库恢复，CASTGC 的价值不该被 12 小时截断。
	credentialTTL time.Duration
	maxSessions   int

	limiter *limiter
	store   *sessionStore
	metrics *metrics

	startedAt time.Time
	adminUser string
	adminPass string
	// trustProxyHeaders 为真时才认 X-Forwarded-For / X-Real-IP：
	// 前面没有自己的反代时，这两个头是客户端随便写的，不能用来标识来源。
	trustProxyHeaders bool

	mu       sync.RWMutex
	sessions map[string]*proxySession

	// dirty 记录自上次快照后被访问过的会话，checkpointLoop 只写这些。
	dirtyMu sync.Mutex
	dirty   map[string]struct{}

	// 用 atomic.Int64 而不是裸 int64：32 位平台上裸 int64 做原子操作会因
	// 字段未按 8 字节对齐直接 panic。
	requests atomic.Int64
}

type proxySession struct {
	id           string
	client       *http.Client
	policy       *proxyPolicy
	jar          *trackingJar
	createdAt    time.Time
	lastAccessAt time.Time
	requests     atomic.Int64

	mu      sync.Mutex
	visited map[string]*url.URL
	// 后台展示用的来源信息。学号来自 CAS 登录表单的 username 字段，
	// 只在 POST /authserver/login 时取一次；密码字段连读都不读。
	remoteIP  string
	userAgent string
	account   string
}

// setMeta 记录来源 IP / UA；空值不覆盖已有内容（后续请求可能没带 UA）。
func (s *proxySession) setMeta(ip, userAgent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ip != "" {
		s.remoteIP = ip
	}
	if userAgent != "" {
		s.userAgent = userAgent
	}
}

func (s *proxySession) setAccount(account string) {
	s.mu.Lock()
	s.account = account
	s.mu.Unlock()
}

func (s *proxySession) meta() (ip, userAgent, account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remoteIP, s.userAgent, s.account
}

type proxyEnvelope struct {
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers"`
	BodyBase64      *string           `json:"bodyBase64"`
	FollowRedirects bool              `json:"followRedirects"`
}

type proxyResponse struct {
	StatusCode     int               `json:"statusCode"`
	ReasonPhrase   string            `json:"reasonPhrase,omitempty"`
	Headers        map[string]string `json:"headers"`
	BodyBase64     string            `json:"bodyBase64"`
	HasActiveLogin bool              `json:"hasActiveLogin"`
}

type sessionRequest struct {
	Reset bool `json:"reset"`
}

// durationEnv 读取形如 "12h"、"90m" 的时长配置；解析失败时保留默认值并告警。
// 用环境变量而不是再加一个 flag，是为了让部署侧不用记一套新的命令行参数。
func durationEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		log.Printf("warning: invalid %s=%q, fallback to %s", name, raw, fallback)
		return fallback
	}
	return parsed
}

// version 由构建脚本注入（-X main.version=...），方便线上排查跑的是哪一版。
var version = "dev"

func main() {
	host := flag.String("host", "0.0.0.0", "listen host")
	port := flag.Int("port", 8081, "listen port")
	allowedTargets := flag.String("allowed-targets", os.Getenv("PROXY_ALLOWED_TARGETS"), "comma-separated targets: exact origin, *.suffix, or CIDR")
	allowedOrigins := flag.String("allowed-origins", os.Getenv("PROXY_ALLOWED_ORIGINS"), "comma-separated browser origins allowed by CORS")
	allowedTargetPorts := flag.String("allowed-target-ports", os.Getenv("PROXY_ALLOWED_TARGET_PORTS"), "comma-separated ports allowed for wildcard/CIDR target rules (default "+defaultAllowedTargetPorts+")")
	storeFile := flag.String("store-file", envOr("PROXY_STORE_FILE", "data/proxy-sessions.db"), "SQLite session store file; empty disables persistence")
	storeKey := flag.String("store-key", os.Getenv("PROXY_STORE_KEY"), "AES-256 key (32 bytes, base64 or hex) used to encrypt the session store")
	storePlaintext := flag.Bool("store-plaintext", os.Getenv("PROXY_STORE_PLAINTEXT") == "1", "allow writing the session store without encryption (not recommended)")
	maxSessions := flag.Int("max-sessions", intEnv("PROXY_MAX_SESSIONS", defaultMaxSessions), "maximum in-memory sessions (LRU eviction)")
	maxStoreEntries := flag.Int("max-store-entries", intEnv("PROXY_MAX_STORE_ENTRIES", defaultMaxStoreEntries), "maximum persisted credential entries")
	rateLimit := flag.Int("rate-limit", intEnv("PROXY_RATE_LIMIT_PER_MINUTE", 120), "per-session requests per minute; 0 disables")
	maxInFlight := flag.Int("max-in-flight", intEnv("PROXY_MAX_IN_FLIGHT", 64), "global concurrent upstream requests; 0 disables")
	adminUser := flag.String("admin-user", os.Getenv("PROXY_ADMIN_USER"), "admin panel username")
	adminPass := flag.String("admin-password", os.Getenv("PROXY_ADMIN_PASSWORD"), "admin panel password")
	trustProxyHeaders := flag.Bool("trust-proxy-headers", os.Getenv("PROXY_TRUST_PROXY_HEADERS") == "1", "trust X-Forwarded-For / X-Real-IP from a local reverse proxy")
	credentialTTL := durationEnv("PROXY_CREDENTIAL_TTL", defaultCredentialTTL)
	sessionTTL := durationEnv("PROXY_SESSION_TTL", defaultSessionTTL)
	flag.Parse()

	policy, err := newProxyPolicyWithPorts(*allowedTargets, *allowedOrigins, *allowedTargetPorts)
	if err != nil {
		log.Fatal(err)
	}
	if len(policy.allowedTargets) == 0 && len(policy.allowedSuffixes) == 0 && len(policy.allowedCIDRs) == 0 {
		log.Printf("warning: no allowed targets configured, all proxy requests will be rejected")
	}

	key, err := parseStoreKey(*storeKey)
	if err != nil {
		log.Fatal(err)
	}
	if *storeFile != "" && len(key) == 0 && !*storePlaintext {
		// 明文落盘等于把 CASTGC 摊在磁盘上，默认不允许；确实需要调试就显式开。
		log.Fatal("PROXY_STORE_KEY is required to persist sessions; pass --store-plaintext to allow plaintext (not recommended)")
	}
	store, err := newSessionStore(*storeFile, key, *maxStoreEntries)
	if err != nil {
		log.Fatal(err)
	}

	server := newProxyServerWithTTL(policy, sessionTTL)
	server.credentialTTL = credentialTTL
	server.maxSessions = *maxSessions
	server.store = store
	server.limiter = newLimiter(*rateLimit, *maxInFlight)
	server.adminUser = *adminUser
	server.adminPass = *adminPass
	server.trustProxyHeaders = *trustProxyHeaders

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go server.cleanupLoop()
	go server.checkpointLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/__proxy__/health", server.handleHealth)
	mux.HandleFunc("/__proxy__/session", server.handleSession)
	mux.HandleFunc("/__proxy__/request", server.handleRequest)
	server.registerAdminRoutes(mux)

	address := fmt.Sprintf("%s:%d", *host, *port)
	stats := store.stats()
	log.Printf(
		"Go proxy %s listening on http://%s (origins=%d suffixes=%d cidrs=%d ports=%d sessionTTL=%s credentialTTL=%s)",
		version,
		address,
		len(policy.allowedTargets),
		len(policy.allowedSuffixes),
		len(policy.allowedCIDRs),
		len(policy.allowedPorts),
		sessionTTL,
		credentialTTL,
	)
	if store.enabled() {
		log.Printf("[store] %s entries=%d encrypted=%t maxEntries=%d", stats.Path, stats.Entries, stats.Encrypted, stats.MaxEntries)
	}

	httpServer := &http.Server{Addr: address, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	// Shutdown 返回后请求已停，补一次落盘；checkpointLoop 里也会写，这里是兜底。
	server.shutdown()
	if store != nil {
		_ = store.close()
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func intEnv(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		log.Printf("warning: invalid %s=%q, fallback to %d", name, raw, fallback)
		return fallback
	}
	return parsed
}

func newProxyServer(policy *proxyPolicy) *proxyServer {
	return newProxyServerWithTTL(policy, defaultSessionTTL)
}

func newProxyServerWithTTL(policy *proxyPolicy, sessionTTL time.Duration) *proxyServer {
	if sessionTTL <= 0 {
		sessionTTL = defaultSessionTTL
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 不走环境变量代理：否则连接检查只能看到代理地址，看不到真实目标。
	transport.Proxy = nil
	transport.DialContext = policy.dialContext
	return &proxyServer{
		policy:        policy,
		transport:     transport,
		sessionTTL:    sessionTTL,
		credentialTTL: defaultCredentialTTL,
		maxSessions:   defaultMaxSessions,
		limiter:       newLimiter(0, 0),
		metrics:       newMetrics(),
		startedAt:     time.Now(),
		sessions:      make(map[string]*proxySession),
		dirty:         make(map[string]struct{}),
	}
}

func (s *proxyServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.applyCORSHeaders(w, r) {
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *proxyServer) handleSession(w http.ResponseWriter, r *http.Request) {
	if !s.applyCORSHeaders(w, r) {
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var req sessionRequest
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "代理请求格式无效"})
			return
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "代理请求格式无效"})
				return
			}
		}
	}

	session := s.ensureSession(w, r, req.Reset)
	hasActiveLogin := session.hasActiveLogin()
	log.Printf("[proxy][session] reset=%t hasActiveLogin=%t", req.Reset, hasActiveLogin)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"hasActiveLogin": hasActiveLogin,
	})
}

func (s *proxyServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	outcome := "ok"
	host := ""
	targetPath := ""
	status := 0
	flagged := false
	defer func() {
		if s.metrics != nil {
			s.metrics.observe(outcome, host, targetPath, status, time.Since(started), flagged)
		}
	}()

	if !s.applyCORSHeaders(w, r) {
		outcome = "origin_denied"
		return
	}
	if r.Method == http.MethodOptions {
		outcome = "preflight"
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodPost {
		outcome = "invalid_request"
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST 代理请求"})
		return
	}

	s.requests.Add(1)
	// 全局并发槽先抢：解析 body 之前就把过载挡在门外，避免大批请求一起读内存。
	if s.limiter != nil && !s.limiter.acquire() {
		outcome = "rate_limited"
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "代理繁忙，请稍后重试"})
		return
	}
	if s.limiter != nil {
		defer s.limiter.release()
	}

	var envelope proxyEnvelope
	if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
		outcome = "invalid_request"
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "代理请求格式无效"})
		return
	}

	method := strings.ToUpper(strings.TrimSpace(envelope.Method))
	rawURL := strings.TrimSpace(envelope.URL)
	if method == "" || rawURL == "" {
		outcome = "invalid_request"
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "代理请求缺少 method 或 url"})
		return
	}

	targetURL, err := url.Parse(rawURL)
	if err != nil || targetURL.Host == "" {
		outcome = "invalid_request"
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "代理目标地址无效"})
		return
	}
	host = strings.ToLower(targetURL.Hostname())
	targetPath = targetURL.Path

	var bodyBytes []byte
	if envelope.BodyBase64 != nil && *envelope.BodyBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(*envelope.BodyBase64)
		if err != nil {
			outcome = "invalid_request"
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "代理请求格式无效"})
			return
		}
		bodyBytes = decoded
	}

	session := s.ensureSession(w, r, false)
	if s.limiter != nil {
		if ok, retry := s.limiter.allow(session.id); !ok {
			outcome = "rate_limited"
			seconds := int(retry.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求过于频繁，请稍后重试"})
			return
		}
	}
	session.requests.Add(1)
	// 目标站看到的 UA 更准确（客户端可能给每个目标单独设置），顺手更新。
	if agent := headerValue(envelope.Headers, "user-agent"); agent != "" {
		session.setMeta("", agent)
	}
	// CAS 登录表单里的 username 是明文：取出来只用于后台显示，密码不读。
	if account := loginAccountFromBody(method, targetURL, bodyBytes); account != "" {
		session.setAccount(account)
		s.markDirty(session.id)
	}
	log.Printf("[proxy][request] method=%s url=%s followRedirects=%t", method, redactURL(rawURL), envelope.FollowRedirects)
	response, err := session.send(method, targetURL, envelope.Headers, bodyBytes, envelope.FollowRedirects)
	if err != nil {
		log.Printf("proxy request failed: %v", err)
		switch {
		case errors.Is(err, errTargetNotAllowed):
			outcome = "target_denied"
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "代理目标不被允许"})
		case isTimeoutError(err):
			outcome = "upstream_timeout"
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "上游响应超时"})
		default:
			outcome = "upstream_error"
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "代理请求失败"})
		}
		return
	}
	status = response.StatusCode
	// 只有「冻结/拦截 + 出口 IP」的冻结页才算被风控；403/429 与登录页都另有归类。
	flagged = detectFrozenPage(response.Body)
	switch {
	case flagged:
		outcome = "upstream_blocked"
	case status >= 500:
		outcome = "upstream_5xx"
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		outcome = "upstream_blocked"
	case status >= 400:
		outcome = "upstream_4xx"
	case isLoginPageHTML(response.Body) && !isAuthserverTarget(targetURL):
		// 业务页跳回登录页：会话失效，需要重新登录，但不是风控。
		outcome = "session_required"
	default:
		outcome = "ok"
	}
	hasActiveLogin := session.hasActiveLogin()
	log.Printf("[proxy][response] method=%s url=%s status=%d hasActiveLogin=%t", method, redactURL(rawURL), response.StatusCode, hasActiveLogin)

	writeJSON(w, http.StatusOK, proxyResponse{
		StatusCode:     response.StatusCode,
		ReasonPhrase:   response.Status,
		Headers:        sanitizeHeaders(response.Header),
		BodyBase64:     base64.StdEncoding.EncodeToString(response.Body),
		HasActiveLogin: hasActiveLogin,
	})
}

// redactURL 抹掉一次性票据与可能的凭据参数，只保留 host/path 用于定位。
// 代理日志会落盘，ST-/code/token 这类参数不该原样留在里面。
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	changed := false
	for key := range query {
		switch strings.ToLower(key) {
		case "ticket", "st", "code", "token", "access_token", "password", "pwd":
			query.Set(key, "***")
			changed = true
		}
	}
	if changed {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

// isTimeoutError 区分「上游不回话」和「连不上/写失败」，后台的异常分类要分开看。
func isTimeoutError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	text := err.Error()
	return strings.Contains(text, "context deadline exceeded") || strings.Contains(text, "Timeout")
}

func (s *proxyServer) ensureSession(w http.ResponseWriter, r *http.Request, reset bool) *proxySession {
	var sessionID string
	if cookie, err := r.Cookie(proxySessionCookieName); err == nil {
		sessionID = cookie.Value
	}
	remoteIP := clientIP(r, s.trustProxyHeaders)
	userAgent := r.UserAgent()

	s.mu.Lock()
	defer s.mu.Unlock()

	session := s.sessions[sessionID]
	if reset {
		if session != nil {
			// reset 保留会话 ID：客户端不需要替换自己手里的 Cookie。
			session.id = sessionID
			session.jar.clear()
			session.ClearVisited()
			session.touch()
			session.setMeta(remoteIP, userAgent)
			// reset 等于退出登录：学号也要清掉，避免后台把新用户显示成上一个。
			session.setAccount("")
			s.sessions[sessionID] = session
			s.store.delete(sessionID)
			s.markDirty(sessionID)
			s.setSessionCookie(w, r, sessionID)
			return session
		}
		// 内存里没有这个会话，凭据库里可能还留着旧登录态，一并清掉再重建。
		s.store.delete(sessionID)
		sessionID = ""
	}

	// 内存没有但凭据库有：同一个浏览器隔天回来时直接用旧 CASTGC 恢复，
	// 这是「12 小时清内存」不会变成「每次都要重新登录」的关键。
	if session == nil && sessionID != "" {
		if entry, ok := s.store.get(sessionID); ok {
			session = s.newProxySession()
			session.jar.restore(entry.Cookies)
			if !entry.CreatedAt.IsZero() {
				session.createdAt = entry.CreatedAt
			}
			if entry.LastSeen.After(session.lastAccessAt) {
				session.lastAccessAt = entry.LastSeen
			}
			session.setMeta(entry.RemoteIP, entry.UserAgent)
			session.setAccount(entry.Account)
			s.sessions[sessionID] = session
		}
	}

	if session == nil {
		s.evictOneLocked()
		sessionID = createSessionID()
		session = s.newProxySession()
		s.sessions[sessionID] = session
	}

	session.touch()
	session.id = sessionID
	session.setMeta(remoteIP, userAgent)
	s.markDirty(sessionID)
	s.setSessionCookie(w, r, sessionID)
	return session
}

// headerValue 在大小写不固定的请求头 map 里按名字取值。
func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// clientIP 取来源地址。默认不信代理头：X-Forwarded-For 可以被客户端随便写，
// 只有前面确实有一层自己人部署的反代时才打开 trustProxy。
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
			if first != "" {
				return first
			}
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
			return realIP
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginAccountFromBody 从 CAS 登录表单里取 username。
//
// 只在「POST 到 ids 的 /authserver/login」这一种情况下看一眼，其余请求体
// 一律不解析；密码字段虽然加密过，也不读取、不记录。取不到就返回空串。
func loginAccountFromBody(method string, target *url.URL, body []byte) string {
	if target == nil || !strings.EqualFold(method, "POST") {
		return ""
	}
	if !strings.HasSuffix(strings.ToLower(target.Hostname()), "ids.njust.edu.cn") {
		return ""
	}
	if !strings.Contains(strings.ToLower(target.Path), "/authserver/login") {
		return ""
	}
	// 登录表单只有几百字节；异常大的 body 直接跳过，避免无谓解析。
	if len(body) == 0 || len(body) > 64*1024 {
		return ""
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(values.Get("username"))
}

// evictOneLocked 在会话数达到上限时淘汰最久未访问的一个。
//
// 这是「清理长时间不用的会话」的主动版本：不等它们熬到 TTL，
// 而是在容量吃紧时先踢最冷的，活跃用户不受影响。
func (s *proxyServer) evictOneLocked() {
	if s.maxSessions <= 0 || len(s.sessions) < s.maxSessions {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, session := range s.sessions {
		if oldestID == "" || session.lastAccessAt.Before(oldest) {
			oldestID = id
			oldest = session.lastAccessAt
		}
	}
	if oldestID == "" {
		return
	}
	if session := s.sessions[oldestID]; session != nil {
		s.persistLocked(oldestID, session)
	}
	delete(s.sessions, oldestID)
	s.limiter.forget(oldestID)
}

// persistLocked 把内存会话的 cookie 交给凭据库；调用方必须持有 s.mu。
func (s *proxyServer) persistLocked(id string, session *proxySession) {
	if s.store == nil || !s.store.enabled() {
		return
	}
	ip, userAgent, account := session.meta()
	s.store.put(storeEntry{
		SessionID: id,
		CreatedAt: session.createdAt,
		LastSeen:  session.lastAccessAt,
		Cookies:   session.jar.snapshot(),
		RemoteIP:  ip,
		UserAgent: userAgent,
		Account:   account,
	})
}

func (s *proxyServer) markDirty(id string) {
	s.dirtyMu.Lock()
	s.dirty[id] = struct{}{}
	s.dirtyMu.Unlock()
}

func (s *proxyServer) setSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string) {
	secureCookie := strings.EqualFold(r.URL.Scheme, "https") ||
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") ||
		r.TLS != nil
	cookie := &http.Cookie{
		Name:     proxySessionCookieName,
		Value:    sessionID,
		HttpOnly: true,
		Path:     "/",
		SameSite: http.SameSiteNoneMode,
		Secure:   secureCookie,
	}
	// 不写 MaxAge 就是会话 Cookie：浏览器一关就丢，凭据库再全也接不上。
	// 有效期取 credentialTTL 而不是 sessionTTL——内存会话可以短，浏览器这枚
	// 身份证必须活得够久，隔几天回来才恢复得出旧罐子。
	ttl := s.credentialTTL
	if ttl <= 0 {
		ttl = s.sessionTTL
	}
	if ttl > 0 {
		cookie.MaxAge = int(ttl.Seconds())
		cookie.Expires = time.Now().Add(ttl)
	}
	// SameSite=None 的 Cookie 必须带 Secure，否则浏览器直接丢弃；
	// 本地用 http 调试时退化成 Lax，至少能让同站请求带上会话。
	if !secureCookie {
		cookie.SameSite = http.SameSiteLaxMode
	}
	http.SetCookie(w, cookie)
}

// ClearVisited 丢掉「见过的路径」记录，reset 时与 cookie 一起清。
func (s *proxySession) ClearVisited() {
	s.mu.Lock()
	s.visited = make(map[string]*url.URL)
	s.mu.Unlock()
}

func (s *proxyServer) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for now := range ticker.C {
		s.cleanup(now)
	}
}

// cleanup 每 10 分钟跑一次：把超时的内存会话转存到凭据库再释放。
//
// 注意顺序：先 persist 再 delete。凭据库留着 CASTGC，用户隔天回来还能
// 恢复出同一个罐子；内存这边则按时释放，不会无限膨胀。
func (s *proxyServer) cleanup(now time.Time) {
	deadline := now.Add(-s.sessionTTL)
	s.mu.Lock()
	for sessionID, session := range s.sessions {
		if session.lastAccessAt.Before(deadline) {
			s.persistLocked(sessionID, session)
			delete(s.sessions, sessionID)
			s.limiter.forget(sessionID)
		}
	}
	s.mu.Unlock()

	if s.store != nil {
		s.store.prune(0)
	}
	if s.limiter != nil {
		s.limiter.gc()
	}
}

// checkpointLoop 定期把被访问过的会话写进凭据库。
//
// 不是每个请求都写盘：20 秒一次、只写 dirty 的会话，崩溃最多丢 20 秒的
// cookie 变化，代价远小于每请求一次 fsync。
func (s *proxyServer) checkpointLoop(ctx context.Context) {
	ticker := time.NewTicker(checkpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case <-ticker.C:
			s.flushDirty()
			if s.store != nil {
				_ = s.store.flush()
			}
		}
	}
}

func (s *proxyServer) flushDirty() {
	s.dirtyMu.Lock()
	if len(s.dirty) == 0 {
		s.dirtyMu.Unlock()
		return
	}
	ids := make([]string, 0, len(s.dirty))
	for id := range s.dirty {
		ids = append(ids, id)
	}
	s.dirty = make(map[string]struct{})
	s.dirtyMu.Unlock()

	log.Printf("[store] checkpoint %d session(s)", len(ids))

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range ids {
		if session := s.sessions[id]; session != nil {
			s.persistLocked(id, session)
		}
	}
}

// shutdown 在进程退出前把内存状态写盘；调用方保证不再有并发请求。
func (s *proxyServer) shutdown() {
	s.flushDirty()
	if s.store != nil {
		_ = s.store.flush()
	}
}

func (s *proxyServer) newProxySession() *proxySession {
	jar := newTrackingJar()
	now := time.Now()
	return &proxySession{
		client: &http.Client{
			Jar:       jar,
			Transport: s.transport,
			Timeout:   40 * time.Second,
		},
		policy:       s.policy,
		jar:          jar,
		createdAt:    now,
		lastAccessAt: now,
		visited:      make(map[string]*url.URL),
	}
}

func (s *proxySession) touch() {
	s.lastAccessAt = time.Now()
}

// hasActiveLogin 只看凭据罐里还有没有非空的 JSESSIONID。
// trackingJar 记录了所有路径下的 cookie，不再需要按 scope 逐个查。
func (s *proxySession) hasActiveLogin() bool {
	return s.jar.hasCookie("JSESSIONID")
}

func (s *proxySession) cookieScopes() []*url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	scopes := append([]*url.URL(nil), s.policy.targetURLs...)
	for _, scope := range s.visited {
		scopes = append(scopes, scope)
	}
	return scopes
}

func (s *proxySession) remember(u *url.URL) {
	segment, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	scope := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/" + segment + "/"}
	s.mu.Lock()
	s.visited[scope.String()] = scope
	s.mu.Unlock()
}

type upstreamResponse struct {
	StatusCode int
	Status     string
	Header     http.Header
	Body       []byte
}

func (s *proxySession) send(method string, targetURL *url.URL, headers map[string]string, bodyBytes []byte, followRedirects bool) (*upstreamResponse, error) {
	if err := s.policy.checkTarget(targetURL); err != nil {
		return nil, err
	}

	var body io.Reader
	if len(bodyBytes) > 0 {
		body = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequest(method, targetURL.String(), body)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		// Host 由目标地址决定，不接受客户端改写，避免绕过目标白名单。
		if strings.EqualFold(key, "Host") {
			continue
		}
		req.Header.Set(key, value)
	}

	client := *s.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !followRedirects {
			return http.ErrUseLastResponse
		}
		if len(via) > maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if err := s.policy.checkTarget(req.URL); err != nil {
			return err
		}
		// 一旦跳到别的源，就不再带客户端自己给的凭据；Cookie 罐按新站点另行附加。
		if leftOrigin(req, via) {
			req.Header.Del("Cookie")
			req.Header.Del("Authorization")
			req.Header.Del("Host")
		}
		s.remember(req.URL)
		return nil
	}

	s.remember(targetURL)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return &upstreamResponse{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Header:     resp.Header.Clone(),
		Body:       bodyData,
	}, nil
}

// leftOrigin 判断这条跳转链是否离开过最初请求的源（scheme+host+port）。
func leftOrigin(req *http.Request, via []*http.Request) bool {
	first, _ := originKey(via[0].URL)
	for _, hop := range via[1:] {
		if key, _ := originKey(hop.URL); key != first {
			return true
		}
	}
	key, _ := originKey(req.URL)
	return key != first
}

func newProxyPolicy(rawTargets, rawOrigins string) (*proxyPolicy, error) {
	return newProxyPolicyWithPorts(rawTargets, rawOrigins, "")
}

// newProxyPolicyWithPorts 解析目标白名单，支持三种写法混用：
//
//	https://bkjw.njust.edu.cn   精确 origin（端口随写法）
//	*.njust.edu.cn / njust.edu.cn  任意子域（含自身），http/https 均可
//	202.119.0.0/16 / 202.119.81.112  按解析后的 IP 判断，端口受限
func newProxyPolicyWithPorts(rawTargets, rawOrigins, rawTargetPorts string) (*proxyPolicy, error) {
	policy := &proxyPolicy{
		allowedTargets: make(map[string]struct{}),
		allowedOrigins: make(map[string]struct{}),
		allowedPorts:   make(map[string]struct{}),
	}
	ports := splitList(rawTargetPorts)
	if len(ports) == 0 {
		ports = splitList(defaultAllowedTargetPorts)
	}
	for _, item := range ports {
		if _, err := strconv.Atoi(item); err != nil {
			return nil, fmt.Errorf("invalid target port %q", item)
		}
		policy.allowedPorts[item] = struct{}{}
	}

	for _, item := range splitList(rawTargets) {
		// 带 scheme 的一律按精确 origin 处理，保持原有语义与端口。
		if strings.Contains(item, "://") {
			key, err := parseOrigin(item)
			if err != nil {
				return nil, fmt.Errorf("invalid allowed target %q: %w", item, err)
			}
			if _, ok := policy.allowedTargets[key]; ok {
				continue
			}
			policy.allowedTargets[key] = struct{}{}
			target, _ := url.Parse(key + "/")
			policy.targetURLs = append(policy.targetURLs, target)
			continue
		}
		// 含 "/" 或本身就是 IP，按地址段处理。
		if prefix, ok, err := parseAddressPattern(item); err != nil {
			return nil, err
		} else if ok {
			policy.allowedCIDRs = append(policy.allowedCIDRs, prefix)
			continue
		}
		// 其余按域名后缀处理：*.njust.edu.cn、.njust.edu.cn、njust.edu.cn 等价。
		suffix := strings.ToLower(strings.TrimSpace(item))
		suffix = strings.TrimPrefix(suffix, "*.")
		suffix = strings.TrimPrefix(suffix, ".")
		if suffix == "" || strings.ContainsAny(suffix, "/*:") {
			return nil, fmt.Errorf("invalid allowed target %q", item)
		}
		policy.allowedSuffixes = append(policy.allowedSuffixes, suffix)
	}

	for _, item := range splitList(rawOrigins) {
		key, err := parseOrigin(item)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed origin %q: %w", item, err)
		}
		policy.allowedOrigins[key] = struct{}{}
	}
	return policy, nil
}

// parseAddressPattern 识别 CIDR 或单个 IP；不是地址写法时返回 ok=false。
func parseAddressPattern(item string) (netip.Prefix, bool, error) {
	if strings.Contains(item, "/") {
		prefix, err := netip.ParsePrefix(item)
		if err != nil {
			return netip.Prefix{}, false, fmt.Errorf("invalid allowed target %q: %w", item, err)
		}
		return prefix.Masked(), true, nil
	}
	if addr, err := netip.ParseAddr(item); err == nil {
		return netip.PrefixFrom(addr, addr.BitLen()), true, nil
	}
	return netip.Prefix{}, false, nil
}

func splitList(raw string) []string {
	var items []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// parseOrigin 把 scheme://host[:port] 规范成带端口的源；不接受路径、查询或通配符。
func parseOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("origin must not contain path, query or fragment")
	}
	return originKey(u)
}

func originKey(u *url.URL) (string, error) {
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("scheme must be http or https")
	}
	if u.User != nil || u.Opaque != "" {
		return "", errors.New("userinfo is not allowed")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.Contains(host, "*") {
		return "", errors.New("host must be an exact name")
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

func (p *proxyPolicy) checkTarget(u *url.URL) error {
	key, err := originKey(u)
	if err != nil {
		return fmt.Errorf("%w: %v", errTargetNotAllowed, err)
	}
	if _, ok := p.allowedTargets[key]; ok {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if p.hostMatchesSuffix(host) && p.portAllowed(u) {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil && p.ipAllowed(ip) && p.portAllowed(u) {
		return nil
	}
	return fmt.Errorf("%w: %s", errTargetNotAllowed, key)
}

func (p *proxyPolicy) hostMatchesSuffix(host string) bool {
	for _, suffix := range p.allowedSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func (p *proxyPolicy) ipAllowed(ip netip.Addr) bool {
	for _, prefix := range p.allowedCIDRs {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// portAllowed 只用于后缀与 CIDR 命中的目标；精确 origin 自带端口。
func (p *proxyPolicy) portAllowed(u *url.URL) bool {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
	}
	_, ok := p.allowedPorts[port]
	return ok
}

func (p *proxyPolicy) allowsOrigin(origin string) bool {
	key, err := parseOrigin(origin)
	if err != nil {
		return false
	}
	_, ok := p.allowedOrigins[key]
	return ok
}

// dialContext 在真正建连时解析并检查地址，再直连检查过的 IP，
// 防止白名单域名被解析（或 DNS 重绑定）到内网。
func (p *proxyPolicy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	literal, literalErr := netip.ParseAddr(host)
	if literalErr == nil {
		// 裸 IP 目标不走「公网即可」的宽松判断：必须显式落在允许的地址段内，
		// 否则白名单写了个域名后缀就等于对外开放了任意 IP。
		if !p.ipAllowed(literal) && !(p.allowLoopback && literal.IsLoopback()) {
			return nil, fmt.Errorf("%w: %s is not in allowed target ranges", errTargetNotAllowed, literal)
		}
		ips = []netip.Addr{literal}
	} else if ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil, err
	}
	for _, ip := range ips {
		// 域名解析结果只要求是公网地址（拒绝回环/内网/CGNAT），
		// 具体允许哪个站由 host 白名单决定。
		if literalErr != nil && !p.isPublicAddr(ip) {
			return nil, fmt.Errorf("%w: %s resolves to %s", errTargetNotAllowed, host, ip)
		}
	}

	var dialer net.Dialer
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no address for %s", host)
	}
	return nil, lastErr
}

func (p *proxyPolicy) isPublicAddr(ip netip.Addr) bool {
	if p.allowLoopback && ip.IsLoopback() {
		return true
	}
	if ip.Is4In6() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func sanitizeHeaders(headers http.Header) map[string]string {
	sanitized := make(map[string]string)
	for key, values := range headers {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if _, ok := passthroughHeaders[normalized]; ok {
			sanitized[normalized] = strings.Join(values, ",")
		}
	}
	return sanitized
}

func createSessionID() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(buf), "=")
}

func writeJSON(w http.ResponseWriter, statusCode int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(value); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("write json failed: %v", err)
	}
}

// applyCORSHeaders 返回 false 表示来源不被允许，已写出 403。
// 没有 Origin 的请求（非浏览器、同源转发）照常放行。
func (s *proxyServer) applyCORSHeaders(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}

	headers := w.Header()
	headers.Add("Vary", "Origin")
	if !s.policy.allowsOrigin(origin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "来源不被允许"})
		return false
	}

	headers.Set("Access-Control-Allow-Origin", origin)
	headers.Set("Access-Control-Allow-Credentials", "true")
	headers.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	headers.Set("Access-Control-Allow-Headers", "Content-Type")
	return true
}
