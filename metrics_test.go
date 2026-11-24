package main

import (
	"net/url"
	"testing"
	"time"
)

func TestMetricsSnapshotAggregates(t *testing.T) {
	metrics := newMetrics()
	metrics.observe("ok", "bkjw.njust.edu.cn", 200, 120*time.Millisecond, false)
	metrics.observe("upstream_timeout", "ids.njust.edu.cn", 0, 3*time.Second, false)
	metrics.observe("upstream_blocked", "bkjw.njust.edu.cn", 403, 80*time.Millisecond, true)

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
		t.Fatal("a single flagged response must not be reported as blocked")
	}
	// 5 分钟内累计 3 次才认定「疑似被拉黑」。
	metrics.observe("upstream_blocked", "bkjw.njust.edu.cn", 429, 50*time.Millisecond, true)
	metrics.observe("upstream_blocked", "bkjw.njust.edu.cn", 403, 50*time.Millisecond, true)
	if !metrics.snapshot().Suspected {
		t.Fatal("three flagged responses within five minutes should be suspected")
	}
}

// 主动登录的请求本身就落在登录页，不能被算成会话被拦。
func TestDetectUpstreamFlagIgnoresOwnLogin(t *testing.T) {
	target, _ := url.Parse("https://ids.njust.edu.cn/authserver/login?service=x")
	if detectUpstreamFlag(200, []byte(`id="pwdEncryptSalt"`), target) {
		t.Fatal("login page fetched on purpose must not count as blocked")
	}
	business, _ := url.Parse("https://bkjw.njust.edu.cn/njlgdx/xskb/xskb_list.do")
	if !detectUpstreamFlag(200, []byte(`<form action="/authserver/login">`), business) {
		t.Fatal("a business page bouncing to the login page is a real session failure")
	}
}

// 时间序列必须补齐 60 个连续分钟，前端画图才不会错位。
func TestMetricsSeriesIsContinuous(t *testing.T) {
	metrics := newMetrics()
	metrics.observe("ok", "bkjw.njust.edu.cn", 200, time.Second, false)
	series := metrics.snapshot().Series
	if len(series) != metricWindowMinutes {
		t.Fatalf("series length = %d, want %d", len(series), metricWindowMinutes)
	}
	if series[len(series)-1].Requests != 1 {
		t.Fatalf("last minute requests = %d, want 1", series[len(series)-1].Requests)
	}
}

func TestDetectUpstreamFlag(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   []byte
		want   bool
	}{
		{"ok", 200, []byte("<html>课表</html>"), false},
		{"forbidden", 403, nil, true},
		{"too many", 429, nil, true},
		{"login page", 200, []byte(`<input id="pwdEncryptSalt">`), true},
		{"authserver redirect", 200, []byte(`/authserver/login?service=x`), true},
		{"gbk freeze page", 200, []byte{0xB6, 0xB3, 0xBD, 0xE1}, true},
	}
	for _, item := range cases {
		if got := detectUpstreamFlag(item.status, item.body, nil); got != item.want {
			t.Errorf("%s: detectUpstreamFlag = %v, want %v", item.name, got, item.want)
		}
	}
}
