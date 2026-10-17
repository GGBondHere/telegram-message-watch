package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/crypto/x509roots/fallback"
	"golang.org/x/text/cases"
)

const version = "1.0.0"
const project = "telegram-message-watch"

var repository = "GGBondHere/telegram-message-watch"

type Config struct {
	APIID             int
	APIHash, BotToken string
	OwnerID, BotID    int64
	Extra             map[string]json.RawMessage
}

func (c *Config) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for key, target := range map[string]any{"api_id": &c.APIID, "api_hash": &c.APIHash, "bot_token": &c.BotToken, "owner_id": &c.OwnerID, "bot_id": &c.BotID} {
		if value, ok := raw[key]; ok {
			if err := json.Unmarshal(value, target); err != nil {
				return fmt.Errorf("配置字段 %s 类型不正确", key)
			}
			delete(raw, key)
		}
	}
	c.Extra = raw
	return nil
}
func (c Config) MarshalJSON() ([]byte, error) {
	raw := map[string]json.RawMessage{}
	for k, v := range c.Extra {
		raw[k] = v
	}
	for k, v := range map[string]any{"api_id": c.APIID, "api_hash": c.APIHash, "bot_token": c.BotToken, "owner_id": c.OwnerID, "bot_id": c.BotID} {
		b, e := json.Marshal(v)
		if e != nil {
			return nil, e
		}
		raw[k] = b
	}
	return json.Marshal(raw)
}

type Chat struct {
	ID    int64
	Title string
}
type Sender struct {
	ID    int64
	Label string
}
type TelegramAccess interface {
	Dialogs(context.Context) ([]Chat, error)
	ResolveSender(context.Context, string) (Sender, error)
}
type Rule struct {
	ID        int64
	Enabled   bool
	SetPaused bool
	Keyword   string
	Sender    Sender
}
type WatchGroup struct {
	ChatID   int64
	Title    string
	Enabled  bool
	Revision int64
	Rules    []Rule
}

func senderLabel(sender Sender) string {
	if sender.Label != "" {
		return sender.Label
	}
	return fmt.Sprint(sender.ID)
}

func (r Rule) description() string {
	if r.Sender.ID == 0 {
		return "消息包含「" + r.Keyword + "」"
	}
	if r.Keyword == "" {
		return senderLabel(r.Sender) + " 的全部消息"
	}
	return senderLabel(r.Sender) + " 发出的含「" + r.Keyword + "」的消息"
}

// Sets are the entries shown in a group: keywords, senders, then one per
// combined sender. Member order follows insertion order within each entry.
type ruleSetKey struct {
	kind     string
	senderID int64
}

func (r Rule) setKey() ruleSetKey {
	if r.Sender.ID == 0 {
		return ruleSetKey{kind: "k"}
	}
	if r.Keyword == "" {
		return ruleSetKey{kind: "s"}
	}
	return ruleSetKey{kind: "b", senderID: r.Sender.ID}
}

type ruleSet struct {
	key    ruleSetKey
	paused bool
	rules  []Rule
}

func groupedRules(rules []Rule) []ruleSet {
	var keywords, senders ruleSet
	var combined []ruleSet
	bySender := map[int64]int{}
	for _, rule := range rules {
		key := rule.setKey()
		var set *ruleSet
		switch key.kind {
		case "k":
			set = &keywords
		case "s":
			set = &senders
		default:
			index, ok := bySender[key.senderID]
			if !ok {
				index = len(combined)
				bySender[key.senderID] = index
				combined = append(combined, ruleSet{})
			}
			set = &combined[index]
		}
		set.key, set.paused = key, rule.SetPaused
		set.rules = append(set.rules, rule)
	}
	var sets []ruleSet
	for _, set := range append([]ruleSet{keywords, senders}, combined...) {
		if len(set.rules) > 0 {
			sets = append(sets, set)
		}
	}
	return sets
}

func (s ruleSet) values() []string {
	values := make([]string, 0, len(s.rules))
	for _, rule := range s.rules {
		value := rule.Keyword
		if s.key.kind == "s" {
			value = senderLabel(rule.Sender)
		}
		values = append(values, value)
	}
	return values
}

func (s ruleSet) heading() string {
	switch s.key.kind {
	case "k":
		return "消息包含"
	case "s":
		return "指定发送者"
	default:
		return senderLabel(s.rules[0].Sender) + " 的消息包含"
	}
}

func (s ruleSet) description() string { return s.heading() + "：" + strings.Join(s.values(), "、") }

func (s ruleSet) active() bool {
	if !s.paused {
		for _, rule := range s.rules {
			if rule.Enabled {
				return true
			}
		}
	}
	return false
}

func ruleCovers(broad, narrow Rule) bool {
	return (broad.Sender.ID == 0 || broad.Sender.ID == narrow.Sender.ID) &&
		(broad.Keyword == "" || narrow.Keyword != "" && strings.Contains(fold(narrow.Keyword), fold(broad.Keyword)))
}

func pauseReason(group WatchGroup, rule Rule, globalEnabled bool) string {
	switch {
	case !globalEnabled:
		return "全部监听已暂停"
	case !group.Enabled:
		return "本群组已暂停"
	case rule.SetPaused:
		return "所属监听项已暂停"
	case !rule.Enabled:
		return "此条件已暂停"
	default:
		return ""
	}
}

// Report only provable inclusion, not incidental intersections. The result is
// informational: it never changes which candidates the user is allowed to save.
func overlapWarnings(group WatchGroup, candidates []Rule, globalEnabled bool) string {
	type warning struct {
		broad, narrow Rule
		larger        bool
		batch         bool
		count         int
	}
	var warnings []warning
	indices := map[string]int{}
	existing := make([]Rule, 0, len(group.Rules)+len(candidates))
	for _, old := range group.Rules {
		replaced := false
		for _, candidate := range candidates {
			replaced = replaced || candidate.ID != 0 && old.ID == candidate.ID
		}
		if !replaced {
			existing = append(existing, old)
		}
	}
	oldCount := len(existing)
	for _, candidate := range candidates {
		if candidate.ID == 0 {
			candidate.Enabled = true
			for _, old := range group.Rules {
				if old.setKey() == candidate.setKey() {
					candidate.SetPaused = old.SetPaused
					break
				}
			}
		}
		for i, old := range existing {
			if old.Sender.ID == candidate.Sender.ID && fold(old.Keyword) == fold(candidate.Keyword) {
				continue
			}
			w := warning{count: 1, batch: i >= oldCount}
			switch {
			case ruleCovers(old, candidate):
				w.broad, w.narrow = old, candidate
			case ruleCovers(candidate, old):
				w.broad, w.narrow, w.larger = candidate, old, true
			default:
				continue
			}
			key := fmt.Sprintf("%t:%t:%d:%s:%s:%s", w.larger, w.batch, w.broad.Sender.ID, fold(w.broad.Keyword), pauseReason(group, w.broad, globalEnabled), pauseReason(group, w.narrow, globalEnabled))
			if index, found := indices[key]; found {
				warnings[index].count++
			} else {
				indices[key] = len(warnings)
				warnings = append(warnings, w)
			}
		}
		existing = append(existing, candidate)
	}
	if len(warnings) == 0 {
		return ""
	}
	text := "\n\n范围提示（不影响保存）："
	for _, w := range warnings[:min(3, len(warnings))] {
		source := "已有"
		if w.batch {
			source = "本批其他"
		}
		broad, narrow := truncateText(w.broad.description(), 70), truncateText(w.narrow.description(), 70)
		line := fmt.Sprintf("%s「%s」包含新增「%s」的范围；取并集，不会收窄原范围。", source, broad, narrow)
		if w.larger {
			line = fmt.Sprintf("新增「%s」包含%s「%s」的范围；取并集，启用后按较大范围提醒。", broad, source, narrow)
		}
		if w.count > 1 {
			line += fmt.Sprintf("涉及 %d 组条件。", w.count)
		}
		if reason := pauseReason(group, w.broad, globalEnabled); reason != "" {
			line += "大范围当前：" + reason + "。"
		}
		if reason := pauseReason(group, w.narrow, globalEnabled); reason != "" {
			line += "小范围当前：" + reason + "。"
		}
		text += "\n• " + truncateText(line, 280)
	}
	if len(warnings) > 3 {
		text += fmt.Sprintf("\n另有 %d 组范围包含，仍可正常保存。", len(warnings)-3)
	}
	return text
}

type Notice struct {
	ChatID, MessageID, OwnerID int64
	Body, Link                 string
	Attempts                   int
	Due, Created, Expires      time.Time
}
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return e.Message }

var tokenPattern = regexp.MustCompile("[0-9]+:[A-Za-z0-9_-]{20,}")
var hashPattern = regexp.MustCompile("^[0-9a-fA-F]{32}$")
var secrets struct {
	sync.RWMutex
	values []string
}

func rememberSecrets(c Config) {
	secrets.Lock()
	defer secrets.Unlock()
	for _, s := range []string{c.APIHash, c.BotToken} {
		if s != "" {
			secrets.values = append(secrets.values, s)
		}
	}
}
func redact(s string) string {
	s = tokenPattern.ReplaceAllString(s, "[Token 已隐藏]")
	secrets.RLock()
	defer secrets.RUnlock()
	for _, value := range secrets.values {
		s = strings.ReplaceAll(s, value, "[凭据已隐藏]")
	}
	return s
}
func logf(format string, args ...any) { log.Print(redact(fmt.Sprintf(format, args...))) }
func fold(s string) string            { return cases.Fold().String(s) }
func truncateText(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	units := 0
	for _, r := range s {
		units++
		if r > 0xffff {
			units++
		}
	}
	if units <= limit {
		return s
	}
	units = 0
	for i, r := range s {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if units+n > limit-1 {
			return s[:i] + "…"
		}
		units += n
	}
	return s
}

func validateConfig(c Config, complete bool) error {
	if c.APIID <= 0 || int64(c.APIID) > 2147483647 {
		return &ConfigError{"API_ID 必须是 1～2147483647 的数字"}
	}
	if !hashPattern.MatchString(c.APIHash) {
		return &ConfigError{"API_HASH 应为 32 位十六进制字符"}
	}
	if !tokenPattern.MatchString(c.BotToken) || tokenPattern.FindString(c.BotToken) != c.BotToken {
		return &ConfigError{"Bot Token 格式不正确"}
	}
	if complete && c.OwnerID <= 0 {
		return &ConfigError{"尚未完成个人账号登录，请运行 tgw，选择「1. 首次配置」"}
	}
	return nil
}
func readConfig(path string, complete bool) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, &ConfigError{"配置不存在或无法读取，请运行 tgw，选择「1. 首次配置」"}
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, &ConfigError{"配置损坏，请运行 tgw，选择「1. 首次配置」，或从「4. 更新与备份」恢复备份"}
	}
	rememberSecrets(c)
	return c, validateConfig(c, complete)
}
func atomicWrite(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".new-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(dir)
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func writeJSON(path string, value any, mode os.FileMode) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	return atomicWrite(path, append(b, '\n'), mode)
}
func copyFile(source, target string, mode os.FileMode) error {
	b, e := os.ReadFile(source)
	if e != nil {
		return e
	}
	return atomicWrite(target, b, mode)
}
func exists(path string) bool  { _, e := os.Stat(path); return e == nil }
func regular(path string) bool { s, e := os.Lstat(path); return e == nil && s.Mode().IsRegular() }
func safeRoot(path string) (string, error) {
	path, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	path = filepath.Clean(path)
	switch path {
	case "/", "/opt", "/etc", "/usr", "/root", "/home", "/var", "/tmp":
		return "", errors.New("安装目录不能是系统共享目录")
	}
	for _, ch := range path {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("/_.-", ch)) {
			return "", errors.New("安装目录仅支持英文字母、数字、斜杠、点、下划线和连字符")
		}
	}
	if s, e := os.Lstat(path); e == nil && s.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("安装目录不能是符号链接")
	}
	return path, nil
}
