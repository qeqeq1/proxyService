package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func testStoreKey() []byte { return bytes.Repeat([]byte{0x2a}, 32) }

func newTestStore(t *testing.T, path string, key []byte, max int) *sessionStore {
	t.Helper()
	store, err := newSessionStore(path, key, max)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	return store
}

func TestStoreRoundTripEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store := newTestStore(t, path, testStoreKey(), 100)
	store.put(storeEntry{
		SessionID: "abc",
		Cookies: []storedCookie{{
			Name:    "CASTGC",
			Value:   "TGT-secret-value",
			Domain:  "ids.njust.edu.cn",
			Path:    "/authserver",
			Expires: time.Now().Add(24 * time.Hour),
		}},
	})
	if err := store.flush(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("TGT-secret-value")) {
		t.Fatal("cookie value leaked into the store file in plaintext")
	}
	if !bytes.HasPrefix(raw, []byte(sqliteHeader)) {
		t.Fatalf("store is not a SQLite database, prefix=%q", raw[:8])
	}
	// Windows 的 os.WriteFile 不按 POSIX 权限位生效，权限断言只在类 Unix 上有意义。
	if info, err := os.Stat(path); err == nil && runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("store file mode = %o, want 600", perm)
		}
	}

	reloaded := newTestStore(t, path, testStoreKey(), 100)
	entry, ok := reloaded.get("abc")
	if !ok {
		t.Fatal("session not restored after reload")
	}
	if len(entry.Cookies) != 1 || entry.Cookies[0].Value != "TGT-secret-value" {
		t.Fatalf("restored cookies = %+v", entry.Cookies)
	}
}

// 密钥不对时不能崩，也不能把文件当成空的覆盖掉；只记警告并停在空库。
func TestStoreWrongKeyStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store := newTestStore(t, path, testStoreKey(), 10)
	store.put(storeEntry{SessionID: "x", Cookies: []storedCookie{{Name: "CASTGC", Value: "v", Domain: "ids.njust.edu.cn", Path: "/"}}})
	if err := store.flush(); err != nil {
		t.Fatal(err)
	}

	other := newTestStore(t, path, bytes.Repeat([]byte{0x11}, 32), 10)
	if _, ok := other.get("x"); ok {
		t.Fatal("store decrypted with a wrong key")
	}
}

// 过期的 cookie 不该被持久化，否则凭据库会一直涨。
func TestStoreDropsExpiredCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store := newTestStore(t, path, testStoreKey(), 10)
	store.put(storeEntry{
		SessionID: "old",
		Cookies: []storedCookie{{
			Name:    "CASTGC",
			Value:   "v",
			Domain:  "ids.njust.edu.cn",
			Path:    "/",
			Expires: time.Now().Add(-time.Minute),
		}},
	})
	if _, ok := store.get("old"); ok {
		t.Fatal("entry with only expired cookies must not be stored")
	}
}

func TestParseStoreKey(t *testing.T) {
	if _, err := parseStoreKey(""); err != nil {
		t.Fatal(err)
	}
	if key, err := parseStoreKey("KioqKioqKioqKioqKioqKioqKioqKioqKioqKioqKio="); err != nil || len(key) != 32 {
		t.Fatalf("base64 key: %v %d", err, len(key))
	}
	if _, err := parseStoreKey("too-short"); err == nil {
		t.Fatal("short key must be rejected")
	}
}
