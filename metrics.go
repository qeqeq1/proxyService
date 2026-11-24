package main

import (
	"bytes"
	"sort"
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
}

func newMetrics() *metrics {
	return &metrics{
		outcomes:  make(map[string]int64),
		targets:   make(map[string]*targetMetric),
		startedAt: time.Now(),
	}
}

// observe 记录一次代理请求。outcome 取值见 admin 页的分组，host 只保留主机名。
func (m *metrics) observe(outcome string, host string, status int, latency time.Duration, flagged bool) {
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

	// 「疑似被拉黑」：最近 5 分钟内出现过 3 次及以上登录页/冻结页/403。
	var recentFlagged int
	cutoff := now.Add(-5 * time.Minute).Unix()
	for _, bucket := range m.buckets {
		if bucket.Minute.Unix() >= cutoff {
			recentFlagged += bucket.Flagged
		}
	}

	snapshot := metricsSnapshot{
		Series:     series,
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

// detectUpstreamFlag 判断这次上游响应是否像「会话不被认可」：
// 403/429、登录页特征、或者教务的 GBK 冻结页。
//
// 只看状态码和几个固定特征串，不解析用户数据，也不保存响应内容。
func detectUpstreamFlag(status int, body []byte) bool {
	if status == 403 || status == 429 {
		return true
	}
	if len(body) == 0 {
		return false
	}
	if len(body) > 512*1024 {
		body = body[:512*1024]
	}
	if bytes.Contains(body, []byte("pwdEncryptSalt")) ||
		bytes.Contains(body, []byte("authserver/login")) {
		return true
	}
	// GBK 编码的「冻结」：教务的 IP 冻结页是 GBK，直接按字节匹配，
	// 避免为了一个判断引入整张 GBK 码表。
	if bytes.Contains(body, []byte{0xB6, 0xB3, 0xBD, 0xE1}) {
		return true
	}
	return false
}
