package main

import (
	"net/url"
	"testing"
	"time"
)

func TestMetricsSnapshotAggregates(t *testing.T) {
	metrics := newMetrics()
	metrics.observe("ok", "bkjw.njust.edu.cn", "/njlgdx/xskb/xskb_list.do", 200, 120*time.Millisecond, false)
	metrics.observe("upstream_timeout", "ids.njust.edu.cn", "/authserver/login", 0, 3*time.Second, false)
	metrics.observe("upstream_blocked", "bkjw.njust.edu.cn", "/njlgdx/indexsso.jsp", 403, 80*time.Millisecond, true)

	snapshot := metrics.snapshot()
	if snapshot.TotalReqs != 3 {
		t.Fatalf("totalRequests = %d, want 3", snapshot.TotalReqs)
	}
	if snapshot.TotalError != 2 {
		t.Fatalf("totalErrors = %d, want 2", snapshot.TotalError)
	}
	if snapshot.Latency.Count != 3 || snapshot.Latency.P95 == 0 {
		t.Fatalf("latency stats = %+v", snapshot.Latency)
	}
	if len(snapshot.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(snapshot.Targets))
	}
	if snapshot.Flagged != 1 {
		t.Fatalf("flagged = %d, want 1", snapshot.Flagged)
	}
	if snapshot.Suspected {
		t.Fatal("a single frozen page must not be reported as repeated")
	}
	metrics.observe("upstream_blocked", "bkjw.njust.edu.cn", "/njlgdx/indexsso.jsp", 429, 50*time.Millisecond, true)
	metrics.observe("upstream_blocked", "bkjw.njust.edu.cn", "/njlgdx/indexsso.jsp", 403, 50*time.Millisecond, true)
	if !metrics.snapshot().Suspected {
		t.Fatal("three frozen pages within five minutes should be suspected")
	}
	// 一次 timeout + 三次 blocked，ok 不进事件。
	if len(metrics.snapshot().Events) != 4 {
		t.Fatalf("events = %d, want 4", len(metrics.snapshot().Events))
	}
}

// 时间序列必须补齐 60 个连续分钟，前端画图才不会错位。
func TestMetricsSeriesIsContinuous(t *testing.T) {
	metrics := newMetrics()
	metrics.observe("ok", "bkjw.njust.edu.cn", "/njlgdx/xskb/xskb_list.do", 200, time.Second, false)
	series := metrics.snapshot().Series
	if len(series) != metricWindowMinutes {
		t.Fatalf("series length = %d, want %d", len(series), metricWindowMinutes)
	}
	if series[len(series)-1].Requests != 1 {
		t.Fatalf("last minute requests = %d, want 1", series[len(series)-1].Requests)
	}
}

// 「被拦截」只认教务的冻结页：冻结/拦截关键字 + 出口 IPv4。
func TestDetectFrozenPage(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{"gbk freeze with ip", []byte{0x31, 0x2e, 0x32, 0x2e, 0x33, 0x2e, 0x34, 0x20, 0xB6, 0xB3, 0xBD, 0xE1}, true},
		{"utf8 block with ip", []byte("你的访问被拦截 10.0.0.8"), true},
		{"login page", []byte(`<input id="pwdEncryptSalt">`), false},
		{"plain ip without keyword", []byte("server 10.0.0.1"), false},
		{"keyword without ip", []byte("账号已冻结，请联系管理员"), false},
		{"empty", nil, false},
	}
	for _, item := range cases {
		if got := detectFrozenPage(item.body); got != item.want {
			t.Errorf("%s: detectFrozenPage = %v, want %v", item.name, got, item.want)
		}
	}
}

// 业务页跳登录页算会话失效，主动登录的 CAS 页面不算。
func TestIsLoginPageAndAuthTarget(t *testing.T) {
	loginBody := []byte(`<form action="/authserver/login">`)
	if !isLoginPageHTML(loginBody) {
		t.Fatal("login page should be detected")
	}
	idsTarget, _ := url.Parse("https://ids.njust.edu.cn/authserver/login?service=x")
	if !isAuthserverTarget(idsTarget) {
		t.Fatal("ids authserver target should be recognised")
	}
	business, _ := url.Parse("https://bkjw.njust.edu.cn/njlgdx/xskb/xskb_list.do")
	if isAuthserverTarget(business) {
		t.Fatal("business target is not an authserver endpoint")
	}
}
