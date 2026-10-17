package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

var (
	ErrRuleChanged   = errors.New("监听设置已改变，请重新打开当前群组")
	ErrDuplicateRule = errors.New("当前群组已存在相同监听项")
	ErrQueueFull     = errors.New("通知队列已满（最多 1000 条）")
)

const (
	storeSchemaVersion = 1
	ruleBatchLimit     = 30
	noticeQueueLimit   = 1000
	noticeLifetime     = 24 * time.Hour
	noticeDedupTime    = 7 * 24 * time.Hour
)

type Store struct {
	db *sql.DB
}

// OpenStore opens the sole SQLite connection used by this process. Every new
// connection receives the connection-local safety settings through its DSN.
func OpenStore(path string) (_ *Store, err error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Chmod(absolute, 0600); err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "synchronous(FULL)")
	// Reserve the writer before reading a revision or matching a message. This
	// also serializes read/modify/write transactions from another Store handle.
	query.Set("_txlock", "immediate")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	if err = initializeStore(db); err != nil {
		return nil, err
	}
	var journalMode string
	if err = db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return nil, err
	}
	if !strings.EqualFold(journalMode, "wal") {
		return nil, fmt.Errorf("SQLite 无法启用 WAL：%s", journalMode)
	}
	return &Store{db: db}, nil
}

func initializeStore(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != storeSchemaVersion {
		return fmt.Errorf("不支持数据库 schema=%d，当前程序仅支持 schema=%d", version, storeSchemaVersion)
	}
	if version == 0 {
		var tables int
		if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return errors.New("数据库没有 schema 版本但已含有表，拒绝覆盖")
		}
		for _, statement := range []string{
			`CREATE TABLE settings (key TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
			`INSERT INTO settings(key,value) VALUES('paused',0)`,
			`CREATE TABLE chats (
                chat_id INTEGER PRIMARY KEY, title TEXT NOT NULL,
                enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
                revision INTEGER NOT NULL DEFAULT 0 CHECK(revision>=0))`,
			`CREATE TABLE rules (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                chat_id INTEGER NOT NULL REFERENCES chats(chat_id) ON DELETE CASCADE,
                enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
                set_paused INTEGER NOT NULL DEFAULT 0 CHECK(set_paused IN (0,1)),
                keyword TEXT NOT NULL DEFAULT '', sender_id INTEGER NOT NULL DEFAULT 0,
                sender_label TEXT NOT NULL DEFAULT '',
                CHECK(keyword<>'' OR sender_id<>0), UNIQUE(chat_id,keyword,sender_id))`,
			`CREATE TABLE notices (
				chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL,
				owner_id INTEGER NOT NULL, body TEXT NOT NULL, link TEXT NOT NULL,
				attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),
				due INTEGER NOT NULL, created INTEGER NOT NULL, expires INTEGER NOT NULL,
				status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','sent','failed','expired')),
				PRIMARY KEY(chat_id,message_id))`,
			`CREATE INDEX notices_due ON notices(status,due,created)`,
			`CREATE INDEX notices_created ON notices(created)`,
			`PRAGMA user_version=1`,
		} {
			if _, err = tx.Exec(statement); err != nil {
				return err
			}
		}
	}
	// Check the columns we depend on even for an existing schema=1 database.
	if _, err = tx.Exec("SELECT chat_id,title,enabled,revision FROM chats LIMIT 0"); err != nil {
		return fmt.Errorf("监听数据库结构不匹配，未修改现有数据：%w", err)
	}
	if _, err = tx.Exec("SELECT id,chat_id,enabled,set_paused,keyword,sender_id,sender_label FROM rules LIMIT 0"); err != nil {
		return fmt.Errorf("监听数据库结构不匹配，未修改现有数据：%w", err)
	}
	if _, err = tx.Exec("SELECT owner_id,body,link,attempts,due,created,expires,status FROM notices LIMIT 0"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

// A group and all its items are read in the same transaction. Its revision
// covers every edit, so stale buttons and pending input cannot overwrite changes.
func readGroup(ctx context.Context, tx *sql.Tx, chatID int64) (WatchGroup, error) {
	group := WatchGroup{ChatID: chatID, Revision: -1}
	err := tx.QueryRowContext(ctx, "SELECT title,enabled,revision FROM chats WHERE chat_id=?", chatID).
		Scan(&group.Title, &group.Enabled, &group.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return group, nil
	}
	if err != nil {
		return group, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,enabled,set_paused,keyword,sender_id,sender_label FROM rules WHERE chat_id=? ORDER BY id", chatID)
	if err != nil {
		return group, err
	}
	defer rows.Close()
	for rows.Next() {
		var rule Rule
		if err = rows.Scan(&rule.ID, &rule.Enabled, &rule.SetPaused, &rule.Keyword, &rule.Sender.ID, &rule.Sender.Label); err != nil {
			return group, err
		}
		group.Rules = append(group.Rules, rule)
	}
	return group, rows.Err()
}

func (s *Store) Group(chatID int64) (WatchGroup, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return WatchGroup{}, err
	}
	defer tx.Rollback()
	group, err := readGroup(context.Background(), tx, chatID)
	if err != nil {
		return WatchGroup{}, err
	}
	return group, tx.Commit()
}

func (s *Store) Groups() ([]WatchGroup, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT chat_id FROM chats ORDER BY title COLLATE NOCASE,chat_id")
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	groups := make([]WatchGroup, 0, len(ids))
	for _, id := range ids {
		group, err := readGroup(context.Background(), tx, id)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, tx.Commit()
}

func (s *Store) SetChat(ctx context.Context, chatID int64, title string) error {
	if chatID == 0 {
		return errors.New("群组 ID 不能为零")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO chats(chat_id,title) VALUES(?,?)
        ON CONFLICT(chat_id) DO UPDATE SET title=excluded.title,revision=chats.revision+1
        WHERE chats.title<>excluded.title`, chatID, strings.TrimSpace(title))
	return err
}

func (s *Store) changeGroup(ctx context.Context, chatID, revision int64, change func(*sql.Tx, *WatchGroup) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	group, err := readGroup(ctx, tx, chatID)
	if err != nil {
		return err
	}
	if group.Revision < 0 || group.Revision != revision {
		return ErrRuleChanged
	}
	if err = change(tx, &group); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE chats SET enabled=?,revision=revision+1 WHERE chat_id=?", group.Enabled, chatID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetGroupEnabled(ctx context.Context, chatID int64, enabled bool, revision int64) error {
	return s.changeGroup(ctx, chatID, revision, func(_ *sql.Tx, group *WatchGroup) error {
		group.Enabled = enabled
		return nil
	})
}

func validRule(rule Rule) error {
	if rule.Keyword == "" && rule.Sender.ID == 0 {
		return errors.New("请填写关键词或发送者")
	}
	if utf8.RuneCountInString(rule.Keyword) > 80 || strings.ContainsAny(rule.Keyword, "\r\n") {
		return errors.New("每项关键词最多 80 字符，不支持换行")
	}
	if rule.Sender.ID < 0 && rule.Sender.ID >= -channelIDOffset {
		return errors.New("发送者 ID 无效")
	}
	return nil
}

// Keep the first occurrence of each condition without changing input order.
func distinctNewRules(existing, candidates []Rule) ([]Rule, int) {
	type key struct {
		keyword string
		sender  int64
	}
	seen := make(map[key]bool, len(existing)+len(candidates))
	for _, rule := range existing {
		seen[key{fold(rule.Keyword), rule.Sender.ID}] = true
	}
	var rules []Rule
	for _, rule := range candidates {
		identity := key{fold(rule.Keyword), rule.Sender.ID}
		if !seen[identity] {
			seen[identity] = true
			rules = append(rules, rule)
		}
	}
	return rules, len(candidates) - len(rules)
}

func (s *Store) AddRules(ctx context.Context, chatID int64, rules []Rule, revision int64) (int, int, error) {
	if len(rules) == 0 || len(rules) > ruleBatchLimit {
		return 0, 0, fmt.Errorf("一次可添加 1～%d 项监听", ruleBatchLimit)
	}
	normalized := append([]Rule(nil), rules...)
	for i := range normalized {
		rule := &normalized[i]
		rule.Keyword = strings.TrimSpace(rule.Keyword)
		rule.Sender.Label = strings.TrimSpace(rule.Sender.Label)
		if rule.ID != 0 {
			return 0, 0, errors.New("新增监听项不能包含已有项目 ID")
		}
		if err := validRule(*rule); err != nil {
			return 0, 0, err
		}
	}
	added, skipped := 0, 0
	err := s.changeGroup(ctx, chatID, revision, func(tx *sql.Tx, group *WatchGroup) error {
		pending, duplicates := distinctNewRules(group.Rules, normalized)
		skipped = duplicates
		if len(pending) == 0 {
			return ErrDuplicateRule // Roll back without changing the revision.
		}
		for _, rule := range pending {
			paused := false
			for _, old := range group.Rules {
				if old.setKey() == rule.setKey() {
					paused = old.SetPaused
					break
				}
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO rules(chat_id,keyword,sender_id,sender_label,set_paused) VALUES(?,?,?,?,?)",
				chatID, rule.Keyword, rule.Sender.ID, rule.Sender.Label, paused); err != nil {
				return err
			}
		}
		added = len(pending)
		return nil
	})
	if errors.Is(err, ErrDuplicateRule) {
		return 0, skipped, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return added, skipped, nil
}

func (s *Store) UpdateRule(ctx context.Context, chatID int64, rule Rule, revision int64) error {
	rule.Keyword = strings.TrimSpace(rule.Keyword)
	rule.Sender.Label = strings.TrimSpace(rule.Sender.Label)
	if err := validRule(rule); err != nil {
		return err
	}
	return s.changeGroup(ctx, chatID, revision, func(tx *sql.Tx, group *WatchGroup) error {
		found := false
		var previous Rule
		for _, old := range group.Rules {
			if old.ID == rule.ID {
				found = true
				previous = old
			} else if fold(old.Keyword) == fold(rule.Keyword) && old.Sender.ID == rule.Sender.ID {
				return ErrDuplicateRule
			}
		}
		if !found {
			return ErrRuleChanged
		}
		paused := previous.SetPaused
		enabled := previous.Enabled
		if previous.setKey() != rule.setKey() {
			// Moving a paused member must not silently activate it in its new set.
			enabled = enabled && !previous.SetPaused
			paused = false
			for _, old := range group.Rules {
				if old.ID != rule.ID && old.setKey() == rule.setKey() {
					paused = old.SetPaused
					break
				}
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE rules SET keyword=?,sender_id=?,sender_label=?,enabled=?,set_paused=? WHERE chat_id=? AND id=?",
			rule.Keyword, rule.Sender.ID, rule.Sender.Label, enabled, paused, chatID, rule.ID)
		return err
	})
}

func (s *Store) changeSet(ctx context.Context, chatID, anchor, revision int64, remove, paused bool) error {
	return s.changeGroup(ctx, chatID, revision, func(tx *sql.Tx, group *WatchGroup) error {
		var key ruleSetKey
		for _, rule := range group.Rules {
			if rule.ID == anchor {
				key = rule.setKey()
				break
			}
		}
		if key.kind == "" {
			return ErrRuleChanged
		}
		for _, rule := range group.Rules {
			if rule.setKey() != key {
				continue
			}
			var err error
			if remove {
				_, err = tx.ExecContext(ctx, "DELETE FROM rules WHERE chat_id=? AND id=?", chatID, rule.ID)
			} else {
				_, err = tx.ExecContext(ctx, "UPDATE rules SET set_paused=? WHERE chat_id=? AND id=?", paused, chatID, rule.ID)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) SetRuleEnabled(ctx context.Context, chatID, id int64, enabled bool, revision int64) error {
	return s.changeGroup(ctx, chatID, revision, func(tx *sql.Tx, group *WatchGroup) error {
		for _, rule := range group.Rules {
			if rule.ID == id {
				_, err := tx.ExecContext(ctx, "UPDATE rules SET enabled=? WHERE chat_id=? AND id=?", enabled, chatID, id)
				return err
			}
		}
		return ErrRuleChanged
	})
}

func (s *Store) DeleteRule(ctx context.Context, chatID, id, revision int64) error {
	return s.changeGroup(ctx, chatID, revision, func(tx *sql.Tx, _ *WatchGroup) error {
		result, err := tx.ExecContext(ctx, "DELETE FROM rules WHERE chat_id=? AND id=?", chatID, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err == nil && count != 1 {
			return ErrRuleChanged
		}
		return err
	})
}

func (s *Store) Pause(ctx context.Context, paused bool) error {
	_, err := s.db.ExecContext(ctx, "UPDATE settings SET value=? WHERE key='paused'", paused)
	return err
}

func (s *Store) Enabled() (bool, error) {
	var paused bool
	err := s.db.QueryRow("SELECT value FROM settings WHERE key='paused'").Scan(&paused)
	return !paused, err
}

func (s *Store) DisableAll() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE chats SET enabled=0,revision=revision+1"); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE notices SET status='failed',body='',link='' WHERE status='pending'"); err != nil {
		return err
	}
	return tx.Commit()
}

func cleanNotices(tx *sql.Tx, now time.Time) error {
	if _, err := tx.Exec("UPDATE notices SET status='expired',body='',link='' WHERE status='pending' AND expires<=?", now.UnixNano()); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM notices WHERE status<>'pending' AND created<=?", now.Add(-noticeDedupTime).UnixNano())
	return err
}

// match only formats cached message data; it must not access the DB or network.
// Matching, pause checks and deduplication share the rule-edit transaction lock.
func (s *Store) Enqueue(n Notice, match func(WatchGroup) string) (bool, error) {
	if n.ChatID == 0 || n.MessageID <= 0 || n.OwnerID <= 0 {
		return false, errors.New("通知的群组、消息或管理员 ID 无效")
	}
	now := time.Now()
	if n.Created.IsZero() || n.Created.After(now) {
		n.Created = now
	}
	latestExpiry := n.Created.Add(noticeLifetime)
	if n.Expires.IsZero() || n.Expires.After(latestExpiry) {
		n.Expires = latestExpiry
	}
	if n.Due.IsZero() {
		n.Due = now
	}
	if n.Attempts < 0 {
		n.Attempts = 0
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = cleanNotices(tx, now); err != nil {
		return false, err
	}
	var exists bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM notices WHERE chat_id=? AND message_id=?)", n.ChatID, n.MessageID).Scan(&exists); err != nil {
		return false, err
	}
	if exists || !n.Expires.After(now) {
		return false, tx.Commit()
	}
	var paused bool
	if err = tx.QueryRow("SELECT value FROM settings WHERE key='paused'").Scan(&paused); err != nil {
		return false, err
	}
	group, err := readGroup(context.Background(), tx, n.ChatID)
	if err != nil {
		return false, err
	}
	if paused || !group.Enabled || match == nil {
		return false, tx.Commit()
	}
	n.Body = match(group)
	if n.Body == "" {
		return false, tx.Commit()
	}
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM notices WHERE status='pending'").Scan(&count); err != nil {
		return false, err
	}
	if count >= noticeQueueLimit {
		// Keep expiry cleanup even if the newly arriving notice cannot fit.
		if err = tx.Commit(); err != nil {
			return false, err
		}
		return false, ErrQueueFull
	}
	_, err = tx.Exec(`INSERT INTO notices(chat_id,message_id,owner_id,body,link,attempts,due,created,expires)
		VALUES(?,?,?,?,?,?,?,?,?)`, n.ChatID, n.MessageID, n.OwnerID, n.Body, n.Link, n.Attempts,
		n.Due.UnixNano(), n.Created.UnixNano(), n.Expires.UnixNano())
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// NextNotice is consumed by the single notification worker. Rows stay pending
// until Sent or Retry so a crash can redeliver, rather than lose, a notice.
func (s *Store) NextNotice(now time.Time) (*Notice, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = cleanNotices(tx, now); err != nil {
		return nil, err
	}
	n := &Notice{}
	var due, created, expires int64
	err = tx.QueryRow(`SELECT chat_id,message_id,owner_id,body,link,attempts,due,created,expires
		FROM notices WHERE status='pending' AND due<=? ORDER BY due,created,chat_id,message_id LIMIT 1`, now.UnixNano()).
		Scan(&n.ChatID, &n.MessageID, &n.OwnerID, &n.Body, &n.Link, &n.Attempts, &due, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	n.Due, n.Created, n.Expires = time.Unix(0, due), time.Unix(0, created), time.Unix(0, expires)
	return n, tx.Commit()
}

func (s *Store) Sent(chatID, messageID int64) error {
	_, err := s.db.Exec("UPDATE notices SET status='sent',body='',link='' WHERE chat_id=? AND message_id=? AND status='pending'", chatID, messageID)
	return err
}

func (s *Store) Retry(n Notice, delay time.Duration, permanent, countAttempt bool) error {
	if delay < 0 {
		delay = 0
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts int
	var expires int64
	err = tx.QueryRow("SELECT attempts,expires FROM notices WHERE chat_id=? AND message_id=? AND status='pending'", n.ChatID, n.MessageID).
		Scan(&attempts, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if countAttempt {
		attempts++
	}
	now := time.Now()
	status := "pending"
	if permanent || attempts >= 5 {
		status = "failed"
	} else if expires <= now.UnixNano() {
		status = "expired"
	}
	if status != "pending" {
		_, err = tx.Exec("UPDATE notices SET attempts=?,status=?,body='',link='' WHERE chat_id=? AND message_id=?", attempts, status, n.ChatID, n.MessageID)
	} else {
		_, err = tx.Exec("UPDATE notices SET attempts=?,due=? WHERE chat_id=? AND message_id=?", attempts, now.Add(delay).UnixNano(), n.ChatID, n.MessageID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// VACUUM INTO reads a SQLite snapshot, including committed WAL pages. It is
// safe both while this Store is open and after another process closed its DB.
func (s *Store) Backup(path string) (err error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(absolute)
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(absolute)
		}
	}()
	if _, err = s.db.Exec("VACUUM main INTO ?", absolute); err != nil {
		return err
	}
	f, err = os.OpenFile(absolute, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
