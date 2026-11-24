package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// storedCookie 是落盘 / 后台展示用的 cookie。
//
// 只保存代理会话自己罐里的东西：没有账号、没有密码，但 CASTGC 这类票据
// 等同于登录态，所以整个快照必须加密存储，日志里也只能出现名字。
type storedCookie struct {
	Name     string    `json:"n"`
	Value    string    `json:"v"`
	Domain   string    `json:"d"`
	Path     string    `json:"p"`
	Expires  time.Time `json:"e,omitempty"`
	Secure   bool      `json:"s,omitempty"`
	HttpOnly bool      `json:"h,omitempty"`
}

// expired 判断这条 cookie 是否已经过期；零值表示会话 cookie（不主动过期）。
func (c storedCookie) expired(now time.Time) bool {
	return !c.Expires.IsZero() && !c.Expires.After(now)
}

// trackingJar 包装标准库 cookiejar：转发读写，同时记下每一条 cookie 的
// 完整属性（域名、路径、过期时间、Secure）。
//
// 标准库的 jar 只能按 URL 查询，看不到全部内容，也无法导出；落盘和后台
// 统计都需要这份清单，所以在写入时顺手记下来。
type trackingJar struct {
	mu    sync.Mutex
	jar   http.CookieJar
	items map[string]storedCookie
}

func newTrackingJar() *trackingJar {
	jar, err := cookiejar.New(nil)
	if err != nil {
		// cookiejar.New 只在传入非法 PublicSuffixList 时失败，这里不会发生。
		panic(err)
	}
	return &trackingJar{jar: jar, items: make(map[string]storedCookie)}
}

func cookieKey(domain, path, name string) string {
	return domain + "\x00" + path + "\x00" + name
}

func (j *trackingJar) SetCookies(target *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jar.SetCookies(target, cookies)
	now := time.Now()
	for _, cookie := range cookies {
		domain := strings.ToLower(strings.TrimPrefix(cookie.Domain, "."))
		if domain == "" {
			domain = strings.ToLower(target.Hostname())
		}
		path := cookie.Path
		if path == "" {
			path = "/"
		}
		key := cookieKey(domain, path, cookie.Name)
		if cookie.MaxAge < 0 {
			delete(j.items, key)
			continue
		}
		stored := storedCookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Domain:   domain,
			Path:     path,
			Secure:   cookie.Secure,
			HttpOnly: cookie.HttpOnly,
		}
		switch {
		case cookie.MaxAge > 0:
			stored.Expires = now.Add(time.Duration(cookie.MaxAge) * time.Second)
		case !cookie.Expires.IsZero():
			stored.Expires = cookie.Expires
		}
		if stored.expired(now) {
			delete(j.items, key)
			continue
		}
		j.items[key] = stored
	}
}

func (j *trackingJar) Cookies(target *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.jar.Cookies(target)
}

// snapshot 返回当前有效（未过期）的 cookie，按域名排序保证输出稳定。
func (j *trackingJar) snapshot() []storedCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	result := make([]storedCookie, 0, len(j.items))
	for key, cookie := range j.items {
		if cookie.expired(now) {
			delete(j.items, key)
			continue
		}
		result = append(result, cookie)
	}
	sort.Slice(result, func(a, b int) bool {
		if result[a].Domain != result[b].Domain {
			return result[a].Domain < result[b].Domain
		}
		if result[a].Path != result[b].Path {
			return result[a].Path < result[b].Path
		}
		return result[a].Name < result[b].Name
	})
	return result
}

// earliestExpiry 返回最早到期的时刻：后台用它提示「这条凭据还能免密多久」。
// 没有带过期时间的 cookie 时返回零值。
func (j *trackingJar) earliestExpiry() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.earliestExpiryLocked(time.Now())
}

func (j *trackingJar) earliestExpiryLocked(now time.Time) time.Time {
	var earliest time.Time
	for _, cookie := range j.items {
		if cookie.expired(now) || cookie.Expires.IsZero() {
			continue
		}
		if earliest.IsZero() || cookie.Expires.Before(earliest) {
			earliest = cookie.Expires
		}
	}
	return earliest
}

// credentialExpiry 优先返回 CASTGC 的到期时间。
//
// CASTGC 才是「免密登录还能用多久」的依据；JSESSIONID 之类的会话 cookie 没有
// 过期时间，拿它们算出来的结果要么是空的，要么是别的 cookie 的寿命，容易误导。
func (j *trackingJar) credentialExpiry() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	for _, cookie := range j.items {
		if cookie.Name != "CASTGC" || cookie.expired(now) || cookie.Expires.IsZero() {
			continue
		}
		return cookie.Expires
	}
	return j.earliestExpiryLocked(now)
}

func (j *trackingJar) hasCookie(name string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	for _, cookie := range j.items {
		if cookie.expired(now) {
			continue
		}
		if cookie.Name == name && cookie.Value != "" {
			return true
		}
	}
	return false
}

func (j *trackingJar) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	total := 0
	for _, cookie := range j.items {
		if !cookie.expired(now) {
			total++
		}
	}
	return total
}

// restore 用落盘的 cookie 重建罐子；调用方保证此时没有有价值的旧内容。
func (j *trackingJar) restore(cookies []storedCookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jar, err := cookiejar.New(nil)
	if err != nil {
		return
	}
	j.jar = jar
	j.items = make(map[string]storedCookie)
	now := time.Now()
	for _, cookie := range cookies {
		if cookie.expired(now) || cookie.Name == "" {
			continue
		}
		scope := &url.URL{Scheme: "https", Host: cookie.Domain, Path: cookie.Path}
		httpCookie := &http.Cookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Domain:   cookie.Domain,
			Path:     cookie.Path,
			Secure:   cookie.Secure,
			HttpOnly: cookie.HttpOnly,
		}
		if !cookie.Expires.IsZero() {
			httpCookie.Expires = cookie.Expires
		}
		jar.SetCookies(scope, []*http.Cookie{httpCookie})
		key := cookieKey(cookie.Domain, cookie.Path, cookie.Name)
		j.items[key] = cookie
	}
}

// clear 丢弃全部 cookie（reset / 登出）。
func (j *trackingJar) clear() {
	j.mu.Lock()
	defer j.mu.Unlock()
	jar, err := cookiejar.New(nil)
	if err != nil {
		j.items = make(map[string]storedCookie)
		return
	}
	j.jar = jar
	j.items = make(map[string]storedCookie)
}
