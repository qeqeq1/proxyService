package main

import (
	"testing"
	"time"
)

func TestLimiterPerSessionWindow(t *testing.T) {
	limiter := newLimiter(2, 0)
	if ok, _ := limiter.allow("a"); !ok {
		t.Fatal("first request must pass")
	}
	if ok, _ := limiter.allow("a"); !ok {
		t.Fatal("second request must pass")
	}
	if ok, retry := limiter.allow("a"); ok || retry <= 0 {
		t.Fatalf("third request must be limited with a retry hint, ok=%v retry=%v", ok, retry)
	}
	// 另一个会话不受影响：限流是按会话算的。
	if ok, _ := limiter.allow("b"); !ok {
		t.Fatal("a different session must not be limited")
	}
}

func TestLimiterWindowRollsOver(t *testing.T) {
	limiter := newLimiter(1, 0)
	limiter.window = 10 * time.Millisecond
	if ok, _ := limiter.allow("a"); !ok {
		t.Fatal("first request must pass")
	}
	if ok, _ := limiter.allow("a"); ok {
		t.Fatal("second request in the same window must be limited")
	}
	time.Sleep(20 * time.Millisecond)
	if ok, _ := limiter.allow("a"); !ok {
		t.Fatal("a new window must reset the counter")
	}
}

func TestLimiterGlobalInFlight(t *testing.T) {
	limiter := newLimiter(0, 1)
	if !limiter.acquire() {
		t.Fatal("first slot must be granted")
	}
	if limiter.acquire() {
		t.Fatal("second slot must be refused")
	}
	limiter.release()
	if !limiter.acquire() {
		t.Fatal("slot must be reusable after release")
	}
	limiter.release()
}

// 会话被清理时计数器也要一起丢，否则 key 会一直堆着。
func TestLimiterForget(t *testing.T) {
	limiter := newLimiter(1, 0)
	limiter.allow("a")
	if stats := limiter.stats(); stats.Tracked != 1 {
		t.Fatalf("tracked = %d, want 1", stats.Tracked)
	}
	limiter.forget("a")
	if stats := limiter.stats(); stats.Tracked != 0 {
		t.Fatalf("tracked = %d, want 0 after forget", stats.Tracked)
	}
}
