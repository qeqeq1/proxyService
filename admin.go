package main

import (
	"crypto/subtle"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// adminStatus 是 /__admin__/api/status 的响应体。
// 只放计数与时间，不放任何 cookie 值。
type adminStatus struct {
	Uptime        string       `json:"uptime"`
	Sessions      int          `json:"sessions"`
	MaxSessions   int          `json:"maxSessions"`
	Requests      int64        `json:"requests"`
	SessionTTL    string       `json:"sessionTtl"`
	CredentialTTL string       `json:"credentialTtl"`
	Store         storeStats   `json:"store"`
	Limiter       limiterStats `json:"limiter"`
}

type sessionRow struct {
	ID          string `json:"id"`
	CreatedAt   string `json:"createdAt"`
	LastSeen    string `json:"lastSeen"`
	IdleSeconds int64  `json:"idleSeconds"`
	Cookies     int    `json:"cookies"`
	HasLogin    bool   `json:"hasLogin"`
	Requests    int64  `json:"requests"`
	Account     string `json:"account,omitempty"`
	RemoteIP    string `json:"remoteIp,omitempty"`
	UserAgent   string `json:"userAgent,omitempty"`
	// Dormant 表示这条只在凭据库里（内存会话已被清理），下次带 cookie 回来会恢复。
	Dormant bool `json:"dormant,omitempty"`
	// ExpiresIn 是罐子里最早到期的 cookie 还剩多久（空字符串表示没有限期）。
	ExpiresIn string `json:"expiresIn,omitempty"`
}

func (s *proxyServer) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/__admin__", s.handleAdminPage)
	mux.HandleFunc("/__admin__/", s.handleAdminPage)
	mux.HandleFunc("/__admin__/api/status", s.handleAdminStatus)
	mux.HandleFunc("/__admin__/api/sessions", s.handleAdminSessions)
	mux.HandleFunc("/__admin__/api/metrics", s.handleAdminMetrics)
	mux.HandleFunc("/__admin__/api/kick", s.handleAdminKick)
	mux.HandleFunc("/__admin__/api/purge", s.handleAdminPurge)
}

// adminAuthorized 在没配置账号密码时只放行回环地址。
// 管理面能看到「哪些会话还登录着」，不该在一个公网端口上裸奔。
func (s *proxyServer) adminAuthorized(r *http.Request) bool {
	if s.adminUser == "" || s.adminPass == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return false
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(user), []byte(s.adminUser)) == 1 &&
		subtle.ConstantTimeCompare([]byte(pass), []byte(s.adminPass)) == 1
}

func (s *proxyServer) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.adminAuthorized(r) {
		return true
	}
	if s.adminUser != "" && s.adminPass != "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="njust proxy admin"`)
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要管理员凭据"})
	return false
}

func (s *proxyServer) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.URL.Path == "/__admin__" {
		// 没有尾斜杠时，页面里的相对路径会解析成 /api/...；补上斜杠再渲染。
		http.Redirect(w, r, "/__admin__/", http.StatusMovedPermanently)
		return
	}
	if r.URL.Path != "/__admin__/" {
		// 其余 /__admin__/* 已经由更具体的路由处理；走到这里说明是未知路径。
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(adminPageHTML))
}

func (s *proxyServer) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.adminStatusSnapshot())
}

func (s *proxyServer) adminStatusSnapshot() adminStatus {
	s.mu.RLock()
	count := len(s.sessions)
	s.mu.RUnlock()
	status := adminStatus{
		Uptime:        time.Since(s.startedAt).Truncate(time.Second).String(),
		Sessions:      count,
		MaxSessions:   s.maxSessions,
		Requests:      s.requests.Load(),
		SessionTTL:    s.sessionTTL.String(),
		CredentialTTL: s.credentialTTL.String(),
		Limiter:       s.limiter.stats(),
	}
	if s.store != nil {
		status.Store = s.store.stats()
	}
	return status
}

func (s *proxyServer) handleAdminMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, metricsSnapshot{})
		return
	}
	writeJSON(w, http.StatusOK, s.metrics.snapshot())
}

func (s *proxyServer) handleAdminSessions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	now := time.Now()
	s.mu.RLock()
	rows := make([]sessionRow, 0, len(s.sessions))
	inMemory := make(map[string]struct{}, len(s.sessions))
	for id, session := range s.sessions {
		ip, userAgent, account := session.meta()
		inMemory[id] = struct{}{}
		row := sessionRow{
			ID:          shortID(id),
			CreatedAt:   session.createdAt.Format("2006-01-02 15:04:05"),
			LastSeen:    session.lastAccessAt.Format("2006-01-02 15:04:05"),
			IdleSeconds: int64(now.Sub(session.lastAccessAt).Seconds()),
			Cookies:     session.jar.count(),
			HasLogin:    session.hasActiveLogin(),
			Requests:    session.requests.Load(),
			Account:     account,
			RemoteIP:    ip,
			UserAgent:   userAgent,
		}
		if expiry := session.jar.earliestExpiry(); !expiry.IsZero() {
			row.ExpiresIn = expiry.Sub(now).Truncate(time.Minute).String()
		}
		rows = append(rows, row)
	}
	s.mu.RUnlock()

	// 内存里没有、凭据库里还留着的条目也列出来：重启后或长时间没人访问时，
	// 后台必须还能看到「这些人手里还有票」，否则等于看不见底盘。
	if s.store != nil {
		for _, entry := range s.store.list() {
			if _, ok := inMemory[entry.SessionID]; ok {
				continue
			}
			row := sessionRow{
				ID:          shortID(entry.SessionID),
				CreatedAt:   entry.CreatedAt.Format("2006-01-02 15:04:05"),
				LastSeen:    entry.LastSeen.Format("2006-01-02 15:04:05"),
				IdleSeconds: int64(now.Sub(entry.LastSeen).Seconds()),
				Cookies:     entry.Cookies,
				HasLogin:    entry.Cookies > 0,
				Account:     entry.Account,
				RemoteIP:    entry.RemoteIP,
				UserAgent:   entry.UserAgent,
				Dormant:     true,
			}
			if !entry.ExpiresAt.IsZero() {
				row.ExpiresIn = entry.ExpiresAt.Sub(now).Truncate(time.Minute).String()
			}
			rows = append(rows, row)
		}
	}

	sort.Slice(rows, func(a, b int) bool { return rows[a].IdleSeconds < rows[b].IdleSeconds })
	writeJSON(w, http.StatusOK, map[string]any{"sessions": rows, "store": s.adminStatusSnapshot().Store})
}

func (s *proxyServer) handleAdminKick(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("id"))
	if target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少会话 id"})
		return
	}
	var kickedID string
	s.mu.Lock()
	for id := range s.sessions {
		if shortID(id) == target || id == target {
			delete(s.sessions, id)
			s.limiter.forget(id)
			kickedID = id
			break
		}
	}
	s.mu.Unlock()

	// 「踢出」必须是彻底清除：只删内存、留着凭据库，用户一刷新就会恢复，
	// 看起来像没踢掉。所以内存和凭据库一起删。
	if kickedID != "" {
		if s.store != nil {
			s.store.remove(kickedID)
			_ = s.store.flush()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "kicked": shortID(kickedID)})
		return
	}
	if s.store != nil {
		if id, ok := s.store.find(target); ok {
			s.store.remove(id)
			_ = s.store.flush()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "kicked": shortID(id), "dormant": true})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "没有这个会话"})
}

// handleAdminPurge 主动踢掉空闲超过 older 的会话，对应「清理长时间不用的会话」。
func (s *proxyServer) handleAdminPurge(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("older"))
	if raw == "" {
		raw = "30m"
	}
	age, err := time.ParseDuration(raw)
	if err != nil || age <= 0 {
		// 允许只传分钟数，如 older=30。
		if minutes, convErr := strconv.Atoi(raw); convErr == nil && minutes > 0 {
			age = time.Duration(minutes) * time.Minute
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "older 需要是时长（如 30m）或分钟数"})
			return
		}
	}
	removed := s.purgeIdle(age)
	if removed > 0 && s.store != nil {
		_ = s.store.flush()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "purged": removed, "older": age.String()})
}

func (s *proxyServer) purgeIdle(age time.Duration) int {
	deadline := time.Now().Add(-age)
	removed := 0
	s.mu.Lock()
	for id, session := range s.sessions {
		if session.lastAccessAt.Before(deadline) {
			s.persistLocked(id, session)
			delete(s.sessions, id)
			s.limiter.forget(id)
			removed++
		}
	}
	s.mu.Unlock()
	return removed
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

const adminPageHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NJUST 代理后台</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 14px/1.6 -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif; margin: 0; padding: 24px; background: #f7f7f8; color: #1c1c1e; }
  h1 { font-size: 18px; margin: 0 0 16px; }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; margin-bottom: 20px; }
  .card { background: #fff; border: 1px solid #e5e5ea; border-radius: 10px; padding: 12px 14px; }
  .card .k { color: #6b6b70; font-size: 12px; }
  .card .v { font-size: 20px; font-variant-numeric: tabular-nums; margin-top: 2px; }
  table { width: 100%; border-collapse: collapse; background: #fff; border: 1px solid #e5e5ea; border-radius: 10px; overflow: hidden; }
  th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid #f0f0f2; font-variant-numeric: tabular-nums; }
  th { font-weight: 600; color: #6b6b70; font-size: 12px; background: #fafafa; }
  tr:last-child td { border-bottom: none; }
  button { font: inherit; padding: 4px 10px; border-radius: 6px; border: 1px solid #d0d0d5; background: #fff; cursor: pointer; }
  button:hover { background: #f2f2f5; }
  .muted { color: #8e8e93; }
  .ua { max-width: 190px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .bar { display: flex; gap: 8px; align-items: center; margin-bottom: 12px; flex-wrap: wrap; }
  .tag { font-size: 12px; padding: 1px 6px; border-radius: 4px; background: #e8f0fe; color: #1a56db; }
  .tag.warn { background: #fff4e5; color: #b45309; }
  .charts { display: grid; grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); gap: 12px; margin: 18px 0 4px; }
  .chart { background: #fff; border: 1px solid #e5e5ea; border-radius: 10px; padding: 10px 12px; }
  .chart h2, h2.sec { font-size: 13px; margin: 0 0 6px; color: #6b6b70; font-weight: 600; }
  .chart svg { width: 100%; height: 92px; display: block; }
  .legend { font-size: 11px; color: #8e8e93; display: flex; gap: 12px; margin-top: 4px; }
  .dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 4px; }
  h2.sec { margin: 20px 0 8px; }
  @media (prefers-color-scheme: dark) {
    body { background: #1c1c1e; color: #f2f2f7; }
    .card, table { background: #2c2c2e; border-color: #3a3a3c; }
    .chart { background: #2c2c2e; border-color: #3a3a3c; }
    h2.sec { color: #a1a1a6; }
    th { background: #242426; color: #a1a1a6; }
    th, td { border-color: #3a3a3c; }
    button { background: #2c2c2e; border-color: #48484a; color: #f2f2f7; }
    button:hover { background: #3a3a3c; }
  }
</style>
</head>
<body>
<h1>NJUST 代理后台</h1>
<div class="cards" id="cards"></div>
<div class="bar">
  <button onclick="purge(30)">清理空闲 30 分钟以上</button>
  <button onclick="purge(120)">清理空闲 2 小时以上</button>
  <button onclick="load()">刷新</button>
  <span class="muted" id="updated"></span>
</div>
<table>
  <thead><tr><th>学号 / 会话</th><th>来源 IP</th><th>UA</th><th>空闲</th><th>Cookie</th><th>登录态</th><th>凭据剩余</th><th>请求</th><th></th></tr></thead>
  <tbody id="rows"><tr><td colspan="9" class="muted">加载中…</td></tr></tbody>
</table>

<div class="charts">
  <div class="chart">
    <h2>每分钟请求（最近 60 分钟）</h2>
    <div id="chartReq"></div>
    <div class="legend">
      <span><span class="dot" style="background:#1a56db"></span>请求</span>
      <span><span class="dot" style="background:#d92d20"></span>异常</span>
    </div>
  </div>
  <div class="chart">
    <h2>平均耗时（毫秒 / 分钟）</h2>
    <div id="chartLat"></div>
    <div class="legend">
      <span><span class="dot" style="background:#0e9384"></span>平均耗时</span>
      <span id="latSummary"></span>
    </div>
  </div>
</div>

<h2 class="sec">上游目标</h2>
<table>
  <thead><tr><th>目标</th><th>请求</th><th>异常</th><th title="响应里出现登录页或 403/429 的次数，不代表封 IP">需重登</th><th>最后状态</th><th>最后请求</th></tr></thead>
  <tbody id="targets"><tr><td colspan="6" class="muted">暂无数据</td></tr></tbody>
</table>

<h2 class="sec">异常分类</h2>
<table>
  <thead><tr><th>类型</th><th>次数</th></tr></thead>
  <tbody id="outcomes"><tr><td colspan="2" class="muted">暂无数据</td></tr></tbody>
</table>
<script>
function fmt(sec) {
  if (sec < 60) return sec + " 秒";
  if (sec < 3600) return Math.floor(sec / 60) + " 分";
  return (sec / 3600).toFixed(1) + " 小时";
}
// 学号与 UA 都来自客户端，必须转义后再拼进 innerHTML。
function esc(value) {
  return String(value == null ? "" : value).replace(/[&<>"']/g, function (ch) {
    return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch];
  });
}
function uaShort(ua) {
  if (!ua) return "—";
  var match = ua.match(/(Edg|Chrome|Firefox|Safari)\/[\d.]+/);
  var os = /Windows/.test(ua) ? "Win" : /Android/.test(ua) ? "Android"
    : /iPhone|iPad/.test(ua) ? "iOS" : /Mac OS X/.test(ua) ? "macOS"
    : /Linux/.test(ua) ? "Linux" : "";
  var head = (match ? match[0] : ua).slice(0, 22);
  return os ? head + " · " + os : head;
}
function card(k, v, warn) {
  return '<div class="card"><div class="k">' + k + '</div><div class="v">' + v +
    (warn ? ' <span class="tag warn">注意</span>' : '') + '</div></div>';
}
// 两条折线共用一套纵轴（请求量级远大于异常数，分开画会把异常压成一条直线）。
function chartTwo(first, second, colorA, colorB) {
  const w = 600, h = 92, pad = 4;
  const max = Math.max(1, Math.max.apply(null, first.concat(second)));
  function line(values, color) {
    const points = values.map(function (v, i) {
      const x = pad + (w - pad * 2) * (i / Math.max(1, values.length - 1));
      const y = h - pad - (h - pad * 2) * (v / max);
      return x.toFixed(1) + "," + y.toFixed(1);
    }).join(" ");
    return '<polyline fill="none" stroke="' + color + '" stroke-width="1.5" points="' + points + '"/>';
  }
  return '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">' +
    line(first, colorA) + line(second, colorB) + "</svg>";
}
function chartOne(values, color) {
  return chartTwo(values, [], color, color);
}
const OUTCOME_LABELS = {
  ok: "成功", invalid_request: "请求格式错误", rate_limited: "本地限流",
  target_denied: "目标被拒", upstream_error: "上游连接失败", upstream_timeout: "上游超时",
  upstream_4xx: "上游 4xx", upstream_5xx: "上游 5xx", upstream_blocked: "上游 403/429",
  session_required: "会话缺失", origin_denied: "来源被拒", preflight: "CORS 预检"
};

async function load() {
  const [status, data, metrics] = await Promise.all([
    fetch("/__admin__/api/status").then(function (r) { return r.json(); }),
    fetch("/__admin__/api/sessions").then(function (r) { return r.json(); }),
    fetch("/__admin__/api/metrics").then(function (r) { return r.json(); })
  ]);
  const store = status.store || {};
  const latency = metrics.latency || {};
  const total = metrics.totalRequests || 0;
  const errorRate = total > 0 ? ((metrics.totalErrors || 0) * 100 / total) : 0;
  document.getElementById("cards").innerHTML =
    card("内存会话", status.sessions + " / " + status.maxSessions) +
    card("凭据库条目", (store.entries || 0) + " / " + (store.maxEntries || "-")) +
    card("即将过期", (store.expiringSoon || 0) + " 条", (store.expiringSoon || 0) > 0) +
    card("近 60 分钟请求", total) +
    card("错误率", errorRate.toFixed(1) + "%", errorRate >= 20) +
    card("p95 耗时", (latency.p95 || 0) + " ms") +
    card("会话状态", metrics.suspected ? "多次需重登" : "正常", metrics.suspected) +
    card("加密", store.encrypted ? "AES-GCM" : "明文", !store.encrypted);

  const series = metrics.series || [];
  document.getElementById("chartReq").innerHTML = chartTwo(
    series.map(function (p) { return p.requests; }),
    series.map(function (p) { return p.errors; }),
    "#1a56db", "#d92d20");
  document.getElementById("chartLat").innerHTML = chartOne(
    series.map(function (p) { return p.avgMs; }), "#0e9384");
  document.getElementById("latSummary").textContent =
    "p50 " + (latency.p50 || 0) + " ms · p95 " + (latency.p95 || 0) +
    " ms · 最大 " + (latency.max || 0) + " ms · 样本 " + (latency.count || 0) +
    (metrics.flagged ? " · 累计被拦 " + metrics.flagged + " 次" : "");

  const targets = metrics.targets || [];
  document.getElementById("targets").innerHTML = targets.length
    ? targets.map(function (t) {
        return "<tr><td>" + esc(t.host) + "</td><td>" + t.requests + "</td><td>" +
          t.errors + "</td><td>" + (t.flagged ? '<span class="tag warn">' + t.flagged + "</span>" : "0") +
          "</td><td>" + (t.lastStatus || "—") + '</td><td class="muted">' + esc(t.lastSeen) + "</td></tr>";
      }).join("")
    : '<tr><td colspan="6" class="muted">最近没有请求</td></tr>';

  const outcomes = Object.keys(metrics.outcomes || {}).map(function (key) {
    return [key, metrics.outcomes[key]];
  }).sort(function (a, b) { return b[1] - a[1]; });
  document.getElementById("outcomes").innerHTML = outcomes.length
    ? outcomes.map(function (entry) {
        return "<tr><td>" + esc(OUTCOME_LABELS[entry[0]] || entry[0]) + "</td><td>" + entry[1] + "</td></tr>";
      }).join("")
    : '<tr><td colspan="2" class="muted">暂无数据</td></tr>';

  const rows = data.sessions || [];
  const tbody = document.getElementById("rows");
  if (!rows.length) {
    tbody.innerHTML = '<tr><td colspan="9" class="muted">当前没有活跃会话</td></tr>';
  } else {
    tbody.innerHTML = rows.map(function (r) {
      const account = r.account
        ? "<strong>" + esc(r.account) + "</strong>"
        : (r.hasLogin ? '<span class="muted">未记录</span>' : '<span class="muted">未登录</span>');
      const state = r.dormant ? ' <span class="tag warn" title="只在凭据库里，带 cookie 回来会恢复">休眠</span>' : "";
      const action = r.dormant ? "清除" : "踢出";
      return "<tr><td>" + account + state +
        '<div class="muted" style="font-size:11px">' + esc(r.id) + "</div></td>" +
        "<td>" + (r.remoteIp ? esc(r.remoteIp) : "—") + "</td>" +
        '<td class="ua" title="' + esc(r.userAgent || "") + '">' + esc(uaShort(r.userAgent)) + "</td>" +
        "<td>" + fmt(r.idleSeconds) + "</td><td>" + r.cookies + "</td>" +
        "<td>" + (r.hasLogin ? '<span class="tag">已登录</span>' : '<span class="muted">无</span>') +
        "</td><td>" + (r.expiresIn || "—") + "</td><td>" + r.requests +
        '</td><td><button onclick="kick(\'' + r.id + '\')">' + action + "</button></td></tr>";
    }).join("");
  }
  document.getElementById("updated").textContent = "更新于 " + new Date().toLocaleTimeString() +
    " · 运行 " + status.uptime + " · 会话 TTL " + status.sessionTtl + " · 凭据 TTL " + status.credentialTtl;
}
async function kick(id) {
  await fetch("/__admin__/api/kick?id=" + encodeURIComponent(id), { method: "POST" });
  load();
}
async function purge(minutes) {
  await fetch("/__admin__/api/purge?older=" + minutes + "m", { method: "POST" });
  load();
}
load();
setInterval(load, 5000);
</script>
</body>
</html>
`
