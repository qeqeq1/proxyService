package main

import (
	"sync"
	"time"
)

// limiter 做两件事：每会话的固定窗口限速、全局并发上限。
//
// 目的不是防攻击（那应该放在前置网关），而是防止单个浏览器把代理打满，
// 以及防止某次前端 bug 触发无限重试时把上游教务拖垮。
type limiter struct {
	mu sync.Mutex

	window time.Duration
	limit  int

	counters map[string]*windowCounter
	lastGC   time.Time

	inFlight    int
	maxInFlight int

	passed   int64
	rejected int64
}

type windowCounter struct {
	start time.Time
	count int
}

func newLimiter(perMinute, maxInFlight int) *limiter {
	return &limiter{
		window:      time.Minute,
		limit:       perMinute,
		maxInFlight: maxInFlight,
		counters:    make(map[string]*windowCounter),
		lastGC:      time.Now(),
	}
}

// allow 返回本次请求是否放行，以及超限时建议的重试等待时间。
func (l *limiter) allow(sessionID string) (bool, time.Duration) {
	if l == nil || l.limit <= 0 {
		return true, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	counter, ok := l.counters[sessionID]
	if !ok || now.Sub(counter.start) >= l.window {
		counter = &windowCounter{start: now}
		l.counters[sessionID] = counter
	}
	if counter.count >= l.limit {
		l.rejected++
		return false, counter.start.Add(l.window).Sub(now)
	}
	counter.count++
	l.passed++
	return true, 0
}

// acquire 抢一个全局并发槽；返回 false 表示目前排队已满。
func (l *limiter) acquire() bool {
	if l == nil || l.maxInFlight <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight >= l.maxInFlight {
		l.rejected++
		return false
	}
	l.inFlight++
	return true
}

func (l *limiter) release() {
	if l == nil || l.maxInFlight <= 0 {
		return
	}
	l.mu.Lock()
	if l.inFlight > 0 {
		l.inFlight--
	}
	l.mu.Unlock()
}

// forget 在会话被清理时同步丢掉计数，避免长尾 key 常驻内存。
func (l *limiter) forget(sessionID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.counters, sessionID)
	l.mu.Unlock()
}

// gc 清理窗口已经过期的计数器，由 cleanupLoop 顺带调用。
func (l *limiter) gc() {
	if l == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) < 10*time.Minute {
		return
	}
	l.lastGC = now
	for key, counter := range l.counters {
		if now.Sub(counter.start) >= l.window {
			delete(l.counters, key)
		}
	}
}

type limiterStats struct {
	Window      time.Duration `json:"window"`
	Limit       int           `json:"limit"`
	InFlight    int           `json:"inFlight"`
	MaxInFlight int           `json:"maxInFlight"`
	Passed      int64         `json:"passed"`
	Rejected    int64         `json:"rejected"`
	Tracked     int           `json:"tracked"`
}

func (l *limiter) stats() limiterStats {
	if l == nil {
		return limiterStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return limiterStats{
		Window:      l.window,
		Limit:       l.limit,
		InFlight:    l.inFlight,
		MaxInFlight: l.maxInFlight,
		Passed:      l.passed,
		Rejected:    l.rejected,
		Tracked:     len(l.counters),
	}
}
