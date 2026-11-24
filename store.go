package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 实现，交叉编译不需要 CGO
)

const (
	// 旧版整文件快照用这两个前缀区分明文/加密；新版用于迁移识别与 payload 加密。
	storeMagicPlain  = "NJP0"
	storeMagicSealed = "NJP1"
	sqliteHeader     = "SQLite format 3\x00"
)

// storeEntry 是一条「浏览器会话 → 上游 Cookie」的长期凭据。
//
// 它和内存里的会话不是一回事：内存会话 12 小时就清（省内存），
// 这里按 cookie 自己的过期时间保存（CASTGC 通常 30 天），用户隔几天
// 回来时还能恢复出同一个罐子，CAS 直接免密放行。
type storeEntry struct {
	SessionID string         `json:"id"`
	CreatedAt time.Time      `json:"created"`
	LastSeen  time.Time      `json:"seen"`
	Cookies   []storedCookie `json:"cookies"`
	// 以下是后台展示用的来源信息，随 payload 一起加密保存，重启后仍能看到。
	RemoteIP  string `json:"ip,omitempty"`
	UserAgent string `json:"ua,omitempty"`
	Account   string `json:"account,omitempty"`
}

// storeFile 只用于读取旧版整文件 JSON 快照（迁移用），新数据不再写这个格式。
type storeFile struct {
	Version int          `json:"version"`
	SavedAt time.Time    `json:"savedAt"`
	Entries []storeEntry `json:"entries"`
}

type storeStats struct {
	Path       string    `json:"path"`
	Entries    int       `json:"entries"`
	Encrypted  bool      `json:"encrypted"`
	MaxEntries int       `json:"maxEntries"`
	LastSaveAt time.Time `json:"lastSaveAt"`
	LastErr    string    `json:"lastErr,omitempty"`
	FileBytes  int64     `json:"fileBytes"`
	// ExpiringSoon 是 72 小时内会过期的条目数，用来提示「这批人要重登了」。
	ExpiringSoon int `json:"expiringSoon"`
}

// storeSummary 是后台展示用的条目摘要，不含 cookie 值。
type storeSummary struct {
	SessionID string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	Cookies   int       `json:"cookies"`
	RemoteIP  string    `json:"remoteIp,omitempty"`
	UserAgent string    `json:"userAgent,omitempty"`
	Account   string    `json:"account,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

// sessionStore 用 SQLite 保存凭据。
//
// 换成 SQLite 的原因：旧版整文件 JSON 是「读全量、改一条、写全量」，
// 条目上万后每 20 秒重写几十 MB，写放大不可接受；SQLite 只改动对应行，
// 还能按 last_seen / expires_at 建索引，清理和排序都不用全表解密。
//
// 安全约定保持不变：
//   - payload 整体 AES-256-GCM 加密，id 与时间戳明文（索引需要）；
//   - 密钥由 PROXY_STORE_KEY 注入，不落盘、不进仓库；
//   - 任何日志与后台输出都不包含 cookie 值。
type sessionStore struct {
	path       string
	key        []byte
	maxEntries int

	db *sql.DB

	mu         sync.Mutex
	lastSaveAt time.Time
	lastErr    string
}

func newSessionStore(path string, key []byte, maxEntries int) (*sessionStore, error) {
	if maxEntries <= 0 {
		maxEntries = defaultMaxStoreEntries
	}
	store := &sessionStore{
		path:       path,
		key:        key,
		maxEntries: maxEntries,
	}
	if path == "" {
		return store, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	// 先把旧版 JSON 读进内存并改名，否则 SQLite 打开时会把它当数据库文件。
	legacy, err := store.readLegacy()
	if err != nil {
		log.Printf("warning: legacy session store not migrated (%v); continuing with an empty database", err)
	}
	if err := store.openDB(); err != nil {
		return nil, err
	}
	for _, entry := range legacy {
		store.put(entry)
	}
	if len(legacy) > 0 {
		if err := store.prune(0); err != nil {
			log.Printf("warning: prune after migration failed: %v", err)
		}
		log.Printf("[store] migrated %d legacy session(s) into %s", len(legacy), path)
	}
	return store, nil
}

func (s *sessionStore) enabled() bool { return s.path != "" }

func (s *sessionStore) openDB() error {
	dsn := "file:" + filepath.ToSlash(s.path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	// 写入都来自同一个进程；单连接可以让 SQL 层的并发问题彻底消失，
	// 凭据库的写入频率也远达不到需要连接池的程度。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  payload    BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_last_seen ON sessions(last_seen);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);
`); err != nil {
		_ = db.Close()
		return err
	}
	s.db = db
	return nil
}

// readLegacy 读取旧版整文件快照；文件本来就是 SQLite 或不存在时返回 nil。
//
// 除了目标路径本身，还会看同名的 .json：默认存储从 proxy-sessions.json 换成
// proxy-sessions.db 之后，线上旧文件正躺在旁边，不主动找一次就等于把凭据库丢了。
func (s *sessionStore) readLegacy() ([]storeEntry, error) {
	for _, path := range s.legacyCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if len(data) == 0 || bytes.HasPrefix(data, []byte(sqliteHeader)) {
			continue
		}
		plain, err := s.open(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var file storeFile
		if err := json.Unmarshal(plain, &file); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if err := os.Rename(path, path+".migrated"); err != nil {
			return nil, err
		}
		log.Printf("[store] migrating legacy snapshot %s", path)
		return file.Entries, nil
	}
	return nil, nil
}

func (s *sessionStore) legacyCandidates() []string {
	paths := []string{s.path}
	if strings.HasSuffix(s.path, ".db") {
		paths = append(paths, strings.TrimSuffix(s.path, ".db")+".json")
	}
	return paths
}

func (s *sessionStore) seal(plain []byte) ([]byte, error) {
	if len(s.key) == 0 {
		return append([]byte(storeMagicPlain), plain...), nil
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	header := append([]byte(storeMagicSealed), nonce...)
	return gcm.Seal(header, nonce, plain, nil), nil
}

func (s *sessionStore) open(data []byte) ([]byte, error) {
	if bytes.HasPrefix(data, []byte(storeMagicPlain)) {
		return data[len(storeMagicPlain):], nil
	}
	if !bytes.HasPrefix(data, []byte(storeMagicSealed)) {
		return nil, errors.New("unknown store format")
	}
	if len(s.key) == 0 {
		return nil, errors.New("store is encrypted but no PROXY_STORE_KEY was provided")
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	body := data[len(storeMagicSealed):]
	if len(body) < gcm.NonceSize() {
		return nil, errors.New("store payload is truncated")
	}
	nonce := body[:gcm.NonceSize()]
	return gcm.Open(nil, nonce, body[gcm.NonceSize():], nil)
}

func earliestExpiry(cookies []storedCookie, now time.Time) int64 {
	var earliest time.Time
	for _, cookie := range cookies {
		if cookie.expired(now) || cookie.Expires.IsZero() {
			continue
		}
		if earliest.IsZero() || cookie.Expires.Before(earliest) {
			earliest = cookie.Expires
		}
	}
	if earliest.IsZero() {
		return 0
	}
	return earliest.Unix()
}

// put 直接落库；SQLite 只改对应行，不需要旧版那种「攒一批再全量重写」。
func (s *sessionStore) put(entry storeEntry) {
	if !s.enabled() || s.db == nil || entry.SessionID == "" {
		return
	}
	now := time.Now()
	live := make([]storedCookie, 0, len(entry.Cookies))
	for _, cookie := range entry.Cookies {
		if cookie.Name != "" && !cookie.expired(now) {
			live = append(live, cookie)
		}
	}
	if len(live) == 0 {
		s.delete(entry.SessionID)
		return
	}
	entry.Cookies = live
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	if entry.LastSeen.IsZero() {
		entry.LastSeen = now
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		s.recordError(err)
		return
	}
	sealed, err := s.seal(payload)
	if err != nil {
		s.recordError(err)
		return
	}
	_, err = s.db.Exec(
		`INSERT INTO sessions (id, created_at, last_seen, expires_at, payload)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   last_seen = excluded.last_seen,
		   expires_at = excluded.expires_at,
		   payload = excluded.payload`,
		entry.SessionID,
		entry.CreatedAt.Unix(),
		entry.LastSeen.Unix(),
		earliestExpiry(live, now),
		sealed,
	)
	if err != nil {
		s.recordError(err)
		return
	}
	s.mu.Lock()
	s.lastSaveAt = time.Now()
	s.lastErr = ""
	s.mu.Unlock()
}

func (s *sessionStore) get(id string) (storeEntry, bool) {
	if !s.enabled() || s.db == nil {
		return storeEntry{}, false
	}
	var payload []byte
	if err := s.db.QueryRow(`SELECT payload FROM sessions WHERE id = ?`, id).Scan(&payload); err != nil {
		return storeEntry{}, false
	}
	plain, err := s.open(payload)
	if err != nil {
		s.recordError(err)
		return storeEntry{}, false
	}
	var entry storeEntry
	if err := json.Unmarshal(plain, &entry); err != nil {
		s.recordError(err)
		return storeEntry{}, false
	}
	return entry, true
}

func (s *sessionStore) delete(id string) {
	if !s.enabled() || s.db == nil {
		return
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id); err != nil {
		s.recordError(err)
	}
}

func (s *sessionStore) remove(id string) bool {
	if !s.enabled() || s.db == nil {
		return false
	}
	result, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		s.recordError(err)
		return false
	}
	affected, err := result.RowsAffected()
	return err == nil && affected > 0
}

// find 按完整 id 或展示用前缀找条目，返回完整 id。
func (s *sessionStore) find(prefix string) (string, bool) {
	if !s.enabled() || s.db == nil || prefix == "" {
		return "", false
	}
	var id string
	if err := s.db.QueryRow(`SELECT id FROM sessions WHERE id = ? OR substr(id, 1, 12) = ?`, prefix, prefix).Scan(&id); err != nil {
		return "", false
	}
	return id, true
}

// list 返回凭据库里的全部条目，按最近使用倒序。
func (s *sessionStore) list() []storeSummary {
	if !s.enabled() || s.db == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT payload FROM sessions ORDER BY last_seen DESC`)
	if err != nil {
		s.recordError(err)
		return nil
	}
	defer rows.Close()
	now := time.Now()
	items := make([]storeSummary, 0, 64)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		plain, err := s.open(payload)
		if err != nil {
			continue
		}
		var entry storeEntry
		if err := json.Unmarshal(plain, &entry); err != nil {
			continue
		}
		summary := storeSummary{
			SessionID: entry.SessionID,
			CreatedAt: entry.CreatedAt,
			LastSeen:  entry.LastSeen,
			Cookies:   len(entry.Cookies),
			RemoteIP:  entry.RemoteIP,
			UserAgent: entry.UserAgent,
			Account:   entry.Account,
		}
		if expiry := earliestExpiry(entry.Cookies, now); expiry > 0 {
			summary.ExpiresAt = time.Unix(expiry, 0)
		}
		items = append(items, summary)
	}
	sort.Slice(items, func(a, b int) bool { return items[a].LastSeen.After(items[b].LastSeen) })
	return items
}

// prune 先按 cookie 过期淘汰，再按最久未用淘汰到上限以内。
func (s *sessionStore) prune(max int) error {
	if !s.enabled() || s.db == nil {
		return nil
	}
	if max <= 0 {
		max = s.maxEntries
	}
	now := time.Now().Unix()
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at > 0 AND expires_at <= ?`, now); err != nil {
		s.recordError(err)
		return err
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		s.recordError(err)
		return err
	}
	if count <= max {
		return nil
	}
	if _, err := s.db.Exec(
		`DELETE FROM sessions WHERE id IN (SELECT id FROM sessions ORDER BY last_seen ASC LIMIT ?)`,
		count-max,
	); err != nil {
		s.recordError(err)
		return err
	}
	return nil
}

// flush 保留给原有调用点：SQLite 每次写入已经提交，这里只做一次 WAL 检查点，
// 避免 WAL 文件无限增长。
func (s *sessionStore) flush() error {
	if !s.enabled() || s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		s.recordError(err)
		return err
	}
	return nil
}

func (s *sessionStore) close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *sessionStore) recordError(err error) {
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
	log.Printf("warning: proxy session store error: %v", err)
}

func (s *sessionStore) stats() storeStats {
	s.mu.Lock()
	lastSave, lastErr := s.lastSaveAt, s.lastErr
	s.mu.Unlock()
	stats := storeStats{
		Path:       s.path,
		Encrypted:  len(s.key) > 0,
		MaxEntries: s.maxEntries,
		LastSaveAt: lastSave,
		LastErr:    lastErr,
	}
	if !s.enabled() || s.db == nil {
		return stats
	}
	now := time.Now()
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&stats.Entries); err != nil {
		stats.LastErr = err.Error()
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE expires_at > 0 AND expires_at <= ?`,
		now.Add(72*time.Hour).Unix(),
	).Scan(&stats.ExpiringSoon); err != nil && stats.LastErr == "" {
		stats.LastErr = err.Error()
	}
	// WAL 文件大小也算进去，后台看到的才是真实磁盘占用。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			stats.FileBytes += info.Size()
		}
	}
	return stats
}

// parseStoreKey 接受 base64 或 hex 的 32 字节密钥。
func parseStoreKey(raw string) ([]byte, error) {
	value := string(bytes.TrimSpace([]byte(raw)))
	if value == "" {
		return nil, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(value); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(value) == 64 {
		decoded := make([]byte, 32)
		if _, err := fmt.Sscanf(value, "%x", &decoded); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("PROXY_STORE_KEY must be 32 bytes encoded as base64 or hex")
}

