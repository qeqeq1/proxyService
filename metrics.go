package main

import (
	"bytes"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// 趋势图保留最近 60 分钟，够看「最近一小时有没有异常」。
	metricWindowMinutes = 60
	// 延时样本用固定大小环形缓冲：算 p50/p95 需要原始值，
	// 但没必要留全部请求，512 条足够稳。
	latencySampleSize = 512
	// 目标维度最多记这么多个 host，防止被随机域名撑爆。
	maxTrackedTargets = 64
	// 异常明细只留最近这么多条，够定位问题又不会无限增长。
	maxErrorEvents = 120
)

type metricBucket struct {
	Minute     time.Time
	Requests   int
	Errors     int
	Timeouts   int
	Flagged    int
	LatencySum int64
	LatencyMax int64
}

// errorEvent 是一条异常/被拦的明细。
//
// 只记 host、路径、状态码和判定类型：不带 query（ticket 之类全在一串参数里），
// 不带响应正文，也不带账号，后台看到的是「哪个目标、哪条路径、为什么算异常」。
type errorEvent struct {
	At      string `json:"at"`
	Host    string `json:"host"`
	Path    string `json:"path"`
	Status  int    `json:"status"`
	Outcome string `json:"outcome"`
	Flagged bool   `json:"flagged"`
}

type targetMetric struct {
	Host       string
	Requests   int64
	Errors     int64
	Flagged    int64
	LastStatus int
	LastSeen   time.Time
}

// metrics 收集后台需要的运行指标。
//
// 刻意只记聚合值：不存 URL、不存 query、不存响应内容，也不存任何用户身份。
// 「被拉黑」这类判断只看状态码和几个页面特征词，够运维定位问题即可。
type metrics struct {
	mu         sync.Mutex
	buckets    []metricBucket
	latencies  []int64
	latencyPos int
	outcomes   map[string]int64
	targets    map[string]*targetMetric
	flagged    int64
	flaggedAt  time.Time
	startedAt  time.Time
	events     []errorEvent
}

func newMetrics() *metrics {
	return &metrics{
		outcomes:  make(map[string]int64),
		targets:   make(map[string]*targetMetric),
		startedAt: time.Now(),
	}
}

// observe 记录一次代理请求。outcome 取值见 admin 页的分组，host 只保留主机名。
func (m *metrics) observe(outcome string, host string, path string, status int, latency time.Duration, flagged bool) {
	if m == nil {
		return
	}
	now := time.Now()
	elapsed := latency.Milliseconds()
	m.mu.Lock()
	defer m.mu.Unlock()

	bucket := m.bucketForLocked(now)
	bucket.Requests++
	bucket.LatencySum += elapsed
	if elapsed > bucket.LatencyMax {
		bucket.LatencyMax = elapsed
	}
	if outcome != "ok" {
		bucket.Errors++
	}
	if outcome == "upstream_timeout" {
		bucket.Timeouts++
	}
	if flagged {
		bucket.Flagged++
		m.flagged++
		m.flaggedAt = now
	}

	m.outcomes[outcome]++
	m.addLatencyLocked(elapsed)

	if (outcome != "ok" && outcome != "preflight") || flagged {
		event := errorEvent{
			At:      now.Format("15:04:05"),
			Host:    host,
			Path:    trimPath(path),
			Status:  status,
			Outcome: outcome,
			Flagged: flagged,
		}
		m.events = append(m.events, event)
		if len(m.events) > maxErrorEvents {
			m.events = m.events[len(m.events)-maxErrorEvents:]
		}
	}

	if host != "" {
		target := m.targets[host]
		if target == nil && len(m.targets) < maxTrackedTargets {
			target = &targetMetric{Host: host}
			m.targets[host] = target
		}
		if target != nil {
			target.Requests++
			if outcome != "ok" {
				target.Errors++
			}
			if flagged {
				target.Flagged++
			}
			if status > 0 {
				target.LastStatus = status
			}
			target.LastSeen = now
		}
	}
}

func (m *metrics) bucketForLocked(now time.Time) *metricBucket {
	minute := now.Truncate(time.Minute)
	if len(m.buckets) == 0 || !m.buckets[len(m.buckets)-1].Minute.Equal(minute) {
		m.buckets = append(m.buckets, metricBucket{Minute: minute})
		if len(m.buckets) > metricWindowMinutes {
			m.buckets = m.buckets[len(m.buckets)-metricWindowMinutes:]
		}
	}
	return &m.buckets[len(m.buckets)-1]
}

func (m *metrics) addLatencyLocked(ms int64) {
	if len(m.latencies) < latencySampleSize {
		m.latencies = append(m.latencies, ms)
		return
	}
	m.latencies[m.latencyPos] = ms
	m.latencyPos = (m.latencyPos + 1) % latencySampleSize
}

type seriesPoint struct {
	Minute   string `json:"minute"`
	Requests int    `json:"requests"`
	Errors   int    `json:"errors"`
	Timeouts int    `json:"timeouts"`
	Flagged  int    `json:"flagged"`
	AvgMs    int64  `json:"avgMs"`
	MaxMs    int64  `json:"maxMs"`
}

type latencyStats struct {
	P50   int64 `json:"p50"`
	P95   int64 `json:"p95"`
	Max   int64 `json:"max"`
	Count int   `json:"count"`
}

type targetRow struct {
	Host       string `json:"host"`
	Requests   int64  `json:"requests"`
	Errors     int64  `json:"errors"`
	Flagged    int64  `json:"flagged"`
	LastStatus int    `json:"lastStatus"`
	LastSeen   string `json:"lastSeen"`
}

type metricsSnapshot struct {
	Series     []seriesPoint  `json:"series"`
	Events     []errorEvent   `json:"events"`
	Latency    latencyStats   `json:"latency"`
	Outcomes   map[string]int64 `json:"outcomes"`
	Targets    []targetRow    `json:"targets"`
	Flagged    int64          `json:"flagged"`
	FlaggedAt  string         `json:"flaggedAt,omitempty"`
	Suspected  bool           `json:"suspected"`
	StartedAt  string         `json:"startedAt"`
	TotalReqs  int64          `json:"totalRequests"`
	TotalError int64          `json:"totalErrors"`
}

// snapshot 生成后台要的完整视图：连续 60 分钟的时间序列（缺的分钟补 0），
// 延时分布，异常分类，以及按目标聚合的健康情况。
func (m *metrics) snapshot() metricsSnapshot {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	byMinute := make(map[int64]metricBucket, len(m.buckets))
	for _, bucket := range m.buckets {
		byMinute[bucket.Minute.Unix()] = bucket
	}
	series := make([]seriesPoint, 0, metricWindowMinutes)
	var totalReqs, totalErrors int64
	start := now.Truncate(time.Minute).Add(-time.Duration(metricWindowMinutes-1) * time.Minute)
	for i := 0; i < metricWindowMinutes; i++ {
		minute := start.Add(time.Duration(i) * time.Minute)
		bucket := byMinute[minute.Unix()]
		point := seriesPoint{Minute: minute.Format("15:04")}
		if bucket.Requests > 0 {
			point.Requests = bucket.Requests
			point.Errors = bucket.Errors
			point.Timeouts = bucket.Timeouts
			point.Flagged = bucket.Flagged
			point.AvgMs = bucket.LatencySum / int64(bucket.Requests)
			point.MaxMs = bucket.LatencyMax
		}
		totalReqs += int64(point.Requests)
		totalErrors += int64(point.Errors)
		series = append(series, point)
	}

	outcomes := make(map[string]int64, len(m.outcomes))
	for key, value := range m.outcomes {
		outcomes[key] = value
	}

	targets := make([]targetRow, 0, len(m.targets))
	for _, target := range m.targets {
		targets = append(targets, targetRow{
			Host:       target.Host,
			Requests:   target.Requests,
			Errors:     target.Errors,
			Flagged:    target.Flagged,
			LastStatus: target.LastStatus,
			LastSeen:   target.LastSeen.Format("15:04:05"),
		})
	}
	sort.Slice(targets, func(a, b int) bool { return targets[a].Requests > targets[b].Requests })

	// 「多次需重登」：最近 5 分钟内出现过 3 次及以上登录页/冻结页/403。
	// 这只说明会话反复失效，不表示 IP 被封；主动登录的请求已排除在外。
	var recentFlagged int
	cutoff := now.Add(-5 * time.Minute).Unix()
	for _, bucket := range m.buckets {
		if bucket.Minute.Unix() >= cutoff {
			recentFlagged += bucket.Flagged
		}
	}

	events := make([]errorEvent, len(m.events))
	copy(events, m.events)
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}

	snapshot := metricsSnapshot{
		Series:     series,
		Events:     events,
		Latency:    m.latencyLocked(),
		Outcomes:   outcomes,
		Targets:    targets,
		Flagged:    m.flagged,
		Suspected:  recentFlagged >= 3,
		StartedAt:  m.startedAt.Format("2006-01-02 15:04:05"),
		TotalReqs:  totalReqs,
		TotalError: totalErrors,
	}
	if !m.flaggedAt.IsZero() {
		snapshot.FlaggedAt = m.flaggedAt.Format("2006-01-02 15:04:05")
	}
	return snapshot
}

// trimPath 把路径压到能看懂的长度；query 在调用方已被排除。
func trimPath(path string) string {
	if path == "" {
		return "/"
	}
	const limit = 64
	if len(path) > limit {
		return path[:limit] + "…"
	}
	return path
}

func (m *metrics) latencyLocked() latencyStats {
	count := len(m.latencies)
	if count == 0 {
		return latencyStats{}
	}
	values := make([]int64, count)
	copy(values, m.latencies)
	sort.Slice(values, func(a, b int) bool { return values[a] < values[b] })
	percentile := func(p float64) int64 {
		index := int(float64(count-1) * p)
		return values[index]
	}
	return latencyStats{
		P50:   percentile(0.50),
		P95:   percentile(0.95),
		Max:   values[count-1],
		Count: count,
	}
}

// 冻结页是唯一的「被拦截」证据：教务会在页面上写「冻结」「拦截」并附出口 IP。
// 编码两种都认：教务的老页面是 GBK，新网关可能是 UTF-8。
var (
	gbkFreeze   = []byte{0xB6, 0xB3, 0xBD, 0xE1} // 冻结
	gbkBlock    = []byte{0xC0, 0xB9, 0xBD, 0xD8} // 拦截
	utf8Freeze  = []byte("冻结")
	utf8Block   = []byte("拦截")
	ipv4Pattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
)

// detectFrozenPage 只认冻结页：同时出现「冻结」或「拦截」以及一个 IPv4 地址。
// 403/429、登录页都不算——那些是会话问题，不是风控。
func detectFrozenPage(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if len(body) > 512*1024 {
		body = body[:512*1024]
	}
	if !ipv4Pattern.Match(body) {
		return false
	}
	return bytes.Contains(body, gbkFreeze) || bytes.Contains(body, utf8Freeze) ||
		bytes.Contains(body, gbkBlock) || bytes.Contains(body, utf8Block)
}

// isLoginPageHTML 判断响应是不是登录页（会话失效时会跳到这里）。
func isLoginPageHTML(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if len(body) > 512*1024 {
		body = body[:512*1024]
	}
	return bytes.Contains(body, []byte("pwdEncryptSalt")) ||
		bytes.Contains(body, []byte("authserver/login"))
}

// isAuthserverTarget 判断目标是不是 CAS 自己的登录接口：
// 主动登录时落在这里是正常的，不能算会话失效。
func isAuthserverTarget(target *url.URL) bool {
	if target == nil {
		return false
	}
	host := strings.ToLower(target.Hostname())
	if host != "ids.njust.edu.cn" {
		return false
	}
	return strings.Contains(strings.ToLower(target.Path), "/authserver/")
}
