package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type binaryManifest struct {
	Project string `json:"project"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func currentManifest() binaryManifest {
	return binaryManifest{project, version, runtime.GOOS, runtime.GOARCH}
}

type Manager struct{ Root string }

func (m Manager) data() string    { return filepath.Join(m.Root, "data") }
func (m Manager) journal() string { return filepath.Join(m.Root, "transaction.json") }
func (m Manager) installed() bool {
	return regular(filepath.Join(m.Root, "installed")) && exists(filepath.Join(m.Root, "current", "tgw"))
}
func fileHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (m Manager) slot() string {
	p, e := os.Readlink(filepath.Join(m.Root, "current"))
	if e != nil {
		return ""
	}
	return filepath.Base(p)
}

var slotPattern = regexp.MustCompile("^[0-9]+\\.[0-9]+\\.[0-9]+-[0-9a-f]{12}$")
var versionPattern = regexp.MustCompile("^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")

func (m Manager) switchSlot(slot string) error {
	path := filepath.Join(m.Root, "current")
	if slot == "" {
		e := os.Remove(path)
		if os.IsNotExist(e) {
			return nil
		}
		return e
	}
	if !slotPattern.MatchString(slot) || !regular(filepath.Join(m.Root, "releases", slot, "tgw")) {
		return errors.New("程序目录标识或内容无效")
	}
	temp := filepath.Join(m.Root, ".current.new")
	os.Remove(temp)
	if e := os.Symlink(filepath.Join("releases", slot), temp); e != nil {
		return e
	}
	if e := os.Rename(temp, path); e != nil {
		return e
	}
	return syncDir(m.Root)
}
func (m Manager) withMaintenance(fn func() error) error {
	if e := m.ensureOwnedRoot(); e != nil {
		return e
	}
	release, e := acquireLock(filepath.Join(m.Root, "maintenance.lock"))
	if e != nil {
		return e
	}
	defer release()
	if e = m.recover(false); e != nil {
		return e
	}
	if e = validateServiceOwnership(m.Root); e != nil {
		return e
	}
	return fn()
}
func consistentCopy(source, target string) error {
	if e := os.MkdirAll(target, 0700); e != nil {
		return e
	}
	entries, e := os.ReadDir(source)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal") {
			continue
		}
		src, dst := filepath.Join(source, name), filepath.Join(target, name)
		if !regular(src) {
			return fmt.Errorf("数据目录含非普通文件：%s", name)
		}
		if name == "state.db" {
			u := url.URL{Scheme: "file", Path: src, RawQuery: "mode=ro"}
			db, e := sql.Open("sqlite", u.String())
			if e != nil {
				return e
			}
			_, e = db.Exec("VACUUM INTO ?", dst)
			closeErr := db.Close()
			if e != nil {
				return e
			}
			if closeErr != nil {
				return closeErr
			}
			if e = os.Chmod(dst, 0600); e != nil {
				return e
			}
			f, e := os.Open(dst)
			if e != nil {
				return e
			}
			e = f.Sync()
			f.Close()
			if e != nil {
				return e
			}
		} else if e := copyFile(src, dst, 0600); e != nil {
			return e
		}
	}
	return syncDir(target)
}

type backupMeta struct {
	ID        string
	CreatedAt time.Time
	Automatic bool
	Slot      string
	Hash      string
	Manifest  binaryManifest
	System    systemSnapshot
	DataFiles map[string]string
}

type backupEntry struct {
	name string
	meta backupMeta
}

func (m Manager) listBackups() ([]backupEntry, error) {
	root := filepath.Join(m.Root, "backups")
	entries, e := os.ReadDir(root)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var backups []backupEntry
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name(), "meta.json")
		if !entry.IsDir() || !regular(path) {
			continue
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		var meta backupMeta
		if json.Unmarshal(b, &meta) != nil || meta.ID == "" || meta.CreatedAt.IsZero() || meta.Slot == "" {
			continue
		}
		backups = append(backups, backupEntry{entry.Name(), meta})
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].meta.CreatedAt.Equal(backups[j].meta.CreatedAt) {
			return backups[i].name < backups[j].name
		}
		return backups[i].meta.CreatedAt.After(backups[j].meta.CreatedAt)
	})
	return backups, nil
}

func validateBackupMeta(meta backupMeta) error {
	if meta.ID == "" || meta.CreatedAt.IsZero() {
		return errors.New("备份缺少标识或创建时间")
	}
	if e := validateSnapshotManager(meta.System); e != nil {
		return e
	}
	if meta.Slot != "" && (!slotPattern.MatchString(meta.Slot) || meta.Manifest.Project != project ||
		meta.Manifest.OS != "linux" || meta.Manifest.Arch != runtime.GOARCH || !versionPattern.MatchString(meta.Manifest.Version)) {
		return errors.New("备份程序的项目、版本或架构与当前服务器不符")
	}
	return nil
}

func (m Manager) backup(automatic bool) (string, error) {
	root := filepath.Join(m.Root, "backups")
	if e := os.MkdirAll(root, 0700); e != nil {
		return "", e
	}
	created := time.Now().UTC()
	dir, e := os.MkdirTemp(root, created.Format("20060102-150405")+"-")
	if e != nil {
		return "", e
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	if e = consistentCopy(m.data(), filepath.Join(dir, "data")); e != nil {
		return "", e
	}
	sys, e := captureSystem(m.Root)
	if e != nil {
		return "", e
	}
	meta := backupMeta{ID: filepath.Base(dir), CreatedAt: created, Automatic: automatic, Slot: m.slot(), System: sys, DataFiles: map[string]string{}}
	dataEntries, e := os.ReadDir(filepath.Join(dir, "data"))
	if e != nil {
		return "", e
	}
	for _, item := range dataEntries {
		hash, e := fileHash(filepath.Join(dir, "data", item.Name()))
		if e != nil {
			return "", e
		}
		meta.DataFiles[item.Name()] = hash
	}
	if meta.Slot != "" {
		if !slotPattern.MatchString(meta.Slot) {
			return "", errors.New("当前程序链接异常")
		}
		binary := filepath.Join(m.Root, "current", "tgw")
		if meta.Hash, e = fileHash(binary); e != nil {
			return "", e
		}
		if e = copyFile(binary, filepath.Join(dir, "program"), 0600); e != nil {
			return "", e
		}
		b, e := os.ReadFile(filepath.Join(m.Root, "current", "manifest.json"))
		if e != nil {
			return "", e
		}
		if e = json.Unmarshal(b, &meta.Manifest); e != nil {
			return "", e
		}
	}
	if e = writeJSON(filepath.Join(dir, "meta.json"), meta, 0600); e != nil {
		return "", e
	}
	success = true
	return dir, nil
}
func (m Manager) restoreSnapshot(backup string) error {
	abs, e := filepath.Abs(backup)
	if e != nil {
		return e
	}
	relative, e := filepath.Rel(filepath.Join(m.Root, "backups"), abs)
	if e != nil || relative == "." || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		return errors.New("备份路径不合法")
	}
	info, e := os.Lstat(abs)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("备份目录无效")
	}
	var meta backupMeta
	b, e := os.ReadFile(filepath.Join(abs, "meta.json"))
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &meta); e != nil {
		return e
	}
	if e = validateBackupMeta(meta); e != nil {
		return e
	}
	dataPath := filepath.Join(abs, "data")
	dataInfo, e := os.Lstat(dataPath)
	if e != nil || !dataInfo.IsDir() || dataInfo.Mode()&os.ModeSymlink != 0 || meta.DataFiles == nil {
		return errors.New("备份数据目录或清单缺失，未修改当前数据")
	}
	entries, e := os.ReadDir(dataPath)
	if e != nil {
		return e
	}
	if len(entries) != len(meta.DataFiles) {
		return errors.New("备份数据文件与清单不符")
	}
	for _, item := range entries {
		p := filepath.Join(dataPath, item.Name())
		if !regular(p) {
			return errors.New("备份含非普通文件")
		}
		hash, e := fileHash(p)
		if e != nil {
			return e
		}
		if hash != meta.DataFiles[item.Name()] {
			return errors.New("备份数据缺失或校验失败")
		}
	}
	if meta.Slot != "" {
		h, e := fileHash(filepath.Join(abs, "program"))
		if e != nil {
			return e
		}
		if h != meta.Hash {
			return errors.New("备份程序校验失败")
		}
		dir := filepath.Join(m.Root, "releases", meta.Slot)
		if e = os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		if e = copyFile(filepath.Join(abs, "program"), filepath.Join(dir, "tgw"), 0755); e != nil {
			return e
		}
		if e = writeJSON(filepath.Join(dir, "manifest.json"), meta.Manifest, 0600); e != nil {
			return e
		}
	}
	stage, discard := filepath.Join(m.Root, ".data.restore"), filepath.Join(m.Root, ".data.discard")
	if e = os.RemoveAll(stage); e != nil {
		return e
	}
	if e = consistentCopy(filepath.Join(abs, "data"), stage); e != nil {
		return e
	}
	if e = os.RemoveAll(discard); e != nil {
		return e
	}
	if exists(m.data()) {
		if e = os.Rename(m.data(), discard); e != nil {
			return e
		}
	}
	if e = os.Rename(stage, m.data()); e != nil {
		return e
	}
	if e = m.switchSlot(meta.Slot); e != nil {
		return e
	}
	if e = restoreSystem(m.Root, meta.System); e != nil {
		return e
	}
	if e = syncDir(m.Root); e != nil {
		return e
	}
	return os.RemoveAll(discard)
}

type transaction struct {
	Backup           string
	Running, Enabled bool
	Runlevels        []string
	Phase            string
	PID              int
	Start            string
}

func processStart(pid int) string {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return ""
	}
	end := strings.LastIndex(string(b), ")")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(b)[end+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}
func ownerAlive(t transaction) bool { return t.Start != "" && processStart(t.PID) == t.Start }
func (m Manager) recover(startup bool) error {
	b, e := os.ReadFile(m.journal())
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var t transaction
	if e = json.Unmarshal(b, &t); e != nil {
		return errors.New("维护事务记录损坏，请检查备份后恢复")
	}
	if startup && ownerAlive(t) {
		if t.Phase == "validating" {
			return nil
		}
		return errors.New("维护仍在修改数据，暂不启动")
	}
	if !startup {
		if e = stopService(); e != nil {
			return e
		}
	}
	if t.Backup != "" {
		release, e := acquireLock(filepath.Join(m.Root, "instance.lock"))
		if e != nil {
			return e
		}
		e = m.restoreSnapshot(t.Backup)
		release()
		if e != nil {
			return e
		}
	}
	t.PID, t.Start, t.Phase = os.Getpid(), processStart(os.Getpid()), "validating"
	if e = writeJSON(m.journal(), t, 0600); e != nil {
		return e
	}
	if e = restoreEnableState(t.Enabled, t.Runlevels); e != nil {
		return e
	}
	if t.Running && !startup {
		if e = startService(m.Root); e != nil {
			return e
		}
	}
	if e = os.Remove(m.journal()); e != nil {
		return e
	}
	fmt.Println("已恢复上次中断操作前的程序和数据。")
	m.pruneAutomaticBackups()
	return nil
}
func (m Manager) operation(change func() error, startAfter, enableAfter bool, history ...bool) error {
	remember := len(history) == 0 || history[0]
	running, enabled := serviceActive(), serviceEnabled()
	t := transaction{Running: running, Enabled: enabled, Phase: "preparing", PID: os.Getpid(), Start: processStart(os.Getpid())}
	if serviceManager == "openrc" {
		t.Runlevels = enabledRunlevels()
	}
	if e := writeJSON(m.journal(), t, 0600); e != nil {
		return e
	}
	fail := func(err error) error {
		if restoreErr := m.recover(false); restoreErr != nil {
			return fmt.Errorf("%w；恢复未完成：%v", err, restoreErr)
		}
		return err
	}
	if e := stopService(); e != nil {
		return fail(e)
	}
	release, e := acquireLock(filepath.Join(m.Root, "instance.lock"))
	if e != nil {
		return fail(e)
	}
	backup, e := m.backup(remember)
	if e != nil {
		release()
		return fail(e)
	}
	t.Backup, t.Phase = backup, "prepared"
	if e = writeJSON(m.journal(), t, 0600); e == nil {
		e = change()
	}
	if e == nil {
		t.Phase = "validating"
		e = writeJSON(m.journal(), t, 0600)
	}
	release()
	if e == nil && enableAfter {
		e = enableService()
	}
	if e == nil && (running || startAfter) {
		e = startService(m.Root)
	}
	if e != nil {
		return fail(e)
	}
	var old backupMeta
	b, e := os.ReadFile(filepath.Join(backup, "meta.json"))
	if e != nil {
		return fail(e)
	}
	if e = json.Unmarshal(b, &old); e != nil {
		return fail(e)
	}
	if remember && old.Slot != "" {
		if e = writeJSON(filepath.Join(m.Root, "last-change.json"), map[string]string{"backup_id": old.ID}, 0600); e != nil {
			return fail(e)
		}
	}
	if e = os.Remove(m.journal()); e != nil {
		return e
	}
	if remember {
		m.prune(old.Slot)
	}
	m.pruneAutomaticBackups()
	return nil
}

func (m Manager) pruneAutomaticBackups() {
	// The maintenance lock is held and the transaction is finished. Never prune
	// explicit manual backups or the snapshot recorded as the rollback target.
	root := filepath.Join(m.Root, "backups")
	entries, e := os.ReadDir(root)
	if os.IsNotExist(e) {
		return
	}
	if e != nil {
		fmt.Println("无法清理旧自动备份：" + redact(e.Error()))
		return
	}
	var rollback map[string]string
	if b, e := os.ReadFile(filepath.Join(m.Root, "last-change.json")); e == nil {
		if json.Unmarshal(b, &rollback) != nil {
			fmt.Println("回滚记录无法读取，保留现有自动备份。")
			return
		}
	} else if !os.IsNotExist(e) {
		fmt.Println("回滚记录无法读取，保留现有自动备份。")
		return
	}
	var backups []backupEntry
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name(), "meta.json")
		if !entry.IsDir() || !regular(path) {
			continue
		}
		b, e := os.ReadFile(path)
		var meta backupMeta
		if e == nil && json.Unmarshal(b, &meta) == nil && meta.Automatic && meta.ID != "" && !meta.CreatedAt.IsZero() {
			backups = append(backups, backupEntry{entry.Name(), meta})
		}
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].meta.CreatedAt.After(backups[j].meta.CreatedAt) })
	for i, backup := range backups {
		if i < 3 || backup.meta.ID == rollback["backup_id"] {
			continue
		}
		if e := os.RemoveAll(filepath.Join(root, backup.name)); e != nil {
			fmt.Println("无法清理旧自动备份：" + redact(e.Error()))
		}
	}
}

func (m Manager) prune(previous string) {
	entries, _ := os.ReadDir(filepath.Join(m.Root, "releases"))
	current := m.slot()
	for _, item := range entries {
		if item.IsDir() && slotPattern.MatchString(item.Name()) && item.Name() != current && item.Name() != previous {
			os.RemoveAll(filepath.Join(m.Root, "releases", item.Name()))
		}
	}
}
func (m Manager) stage(binary string, meta binaryManifest) (string, error) {
	digest, e := fileHash(binary)
	if e != nil {
		return "", e
	}
	slot := meta.Version + "-" + digest[:12]
	dir := filepath.Join(m.Root, "releases", slot)
	if regular(filepath.Join(dir, "tgw")) {
		h, e := fileHash(filepath.Join(dir, "tgw"))
		if e != nil || h != digest {
			return "", errors.New("程序目录内容冲突")
		}
		return slot, nil
	}
	if e = os.MkdirAll(filepath.Dir(dir), 0700); e != nil {
		return "", e
	}
	temp, e := os.MkdirTemp(filepath.Dir(dir), ".stage-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(temp)
	if e = copyFile(binary, filepath.Join(temp, "tgw"), 0755); e != nil {
		return "", e
	}
	if e = writeJSON(filepath.Join(temp, "manifest.json"), meta, 0600); e != nil {
		return "", e
	}
	if e = os.Rename(temp, dir); e != nil {
		return "", e
	}
	return slot, syncDir(filepath.Dir(dir))
}
func (m Manager) installProgram(binary string, meta binaryManifest, register bool) error {
	slot, e := m.stage(binary, meta)
	if e != nil {
		return e
	}
	if slot == m.slot() && !register {
		fmt.Println("已安装此程序。")
		return nil
	}
	return m.operation(func() error {
		if e := m.switchSlot(slot); e != nil {
			return e
		}
		if register {
			if e := installService(m.Root); e != nil {
				return e
			}
			if e := atomicWrite(filepath.Join(m.Root, "installed"), []byte(project+"\n"), 0600); e != nil {
				return e
			}
		}
		return runCandidate(filepath.Join(m.Root, "releases", slot, "tgw"), "--root", m.Root, "migrate")
	}, false, false)
}
func (m Manager) Install() error {
	if e := validatePlatform(); e != nil {
		return e
	}
	if e := validateServiceOwnership(m.Root); e != nil {
		return e
	}
	source, e := os.Executable()
	if e != nil {
		return e
	}
	source, e = filepath.EvalSymlinks(source)
	if e != nil {
		return e
	}
	current := filepath.Join(m.Root, "current", "tgw")
	link, _ := os.Readlink(shortcutPath)
	if m.installed() && regular(servicePath()) && link == filepath.Join(m.Root, "current", "tgw") {
		a, _ := fileHash(source)
		b, _ := fileHash(current)
		if a != "" && a == b {
			fmt.Printf("已安装 Telegram Message Watch %s，无需重复安装。\n", version)
			shortcutHint()
			return nil
		}
	}
	if e = os.MkdirAll(m.Root, 0700); e != nil {
		return e
	}
	if e = os.Chmod(m.Root, 0700); e != nil {
		return e
	}
	if e = m.installProgram(source, currentManifest(), true); e != nil {
		return e
	}
	fmt.Println("程序安装完成，已注册服务；尚未完成首次配置时不会启动服务。")
	shortcutHint()
	return nil
}
func runCandidate(binary string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if e != nil {
		return fmt.Errorf("候选程序检查失败：%s", redact(strings.TrimSpace(string(out))))
	}
	return nil
}
func validateBinary(path string) (binaryManifest, error) {
	var meta binaryManifest
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e := os.Chmod(path, 0755); e != nil {
		return meta, e
	}
	b, e := exec.CommandContext(ctx, path, "--manifest").Output()
	if e != nil {
		return meta, errors.New("无法执行候选程序，检查架构和文件完整性")
	}
	if e = json.Unmarshal(b, &meta); e != nil {
		return meta, errors.New("候选程序元数据无效")
	}
	if meta.Project != project || meta.OS != "linux" || meta.Arch != runtime.GOARCH || !versionPattern.MatchString(meta.Version) {
		return meta, errors.New("候选程序项目、平台、架构或版本不匹配")
	}
	return meta, runCandidate(path, "--self-check")
}
func (m Manager) Setup(field string) error {
	setupCtx, stopSetup := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSetup()
	path := filepath.Join(m.data(), "config.json")
	var old Config
	if b, e := os.ReadFile(path); e == nil {
		if e = json.Unmarshal(b, &old); e != nil {
			yes, askErr := confirmContext(setupCtx, "配置已损坏，保留备份后重新初始化？")
			if askErr != nil {
				return askErr
			}
			if !yes {
				return errCancelled
			}
		}
	}
	candidate := old
	if old.OwnerID == 0 && field == "" {
		if b, e := os.ReadFile(filepath.Join(m.data(), "setup-draft.json")); e == nil {
			json.Unmarshal(b, &candidate)
		}
	}
	for _, key := range []string{"api_id", "api_hash", "bot_token"} {
		if field != "" && field != key {
			continue
		}
		present := key == "api_id" && candidate.APIID > 0 || key == "api_hash" && candidate.APIHash != "" || key == "bot_token" && candidate.BotToken != ""
		if field == "" && present {
			continue
		}
		for {
			value, e := promptContext(setupCtx, map[string]string{"api_id": "API_ID：", "api_hash": "API_HASH：", "bot_token": "Bot Token："}[key], key != "api_id")
			if e != nil {
				return e
			}
			valid := false
			switch key {
			case "api_id":
				n, e := strconv.ParseInt(value, 10, 32)
				if e == nil && n > 0 {
					candidate.APIID = int(n)
					valid = true
				}
			case "api_hash":
				if hashPattern.MatchString(value) {
					candidate.APIHash = value
					valid = true
				}
			case "bot_token":
				if tokenPattern.FindString(value) == value && value != "" {
					candidate.BotToken = value
					valid = true
				}
			}
			if valid {
				break
			}
			fmt.Println("格式不正确，请重新输入。")
		}
		rememberSecrets(candidate)
		if old.OwnerID == 0 && field == "" {
			if e := writeJSON(filepath.Join(m.data(), "setup-draft.json"), candidate, 0600); e != nil {
				return e
			}
		}
	}
	if e := validateConfig(candidate, false); e != nil {
		return e
	}
	rememberSecrets(candidate)
	ctx, cancel := context.WithTimeout(setupCtx, 30*time.Second)
	botID, user, e := NewBot(candidate.BotToken).Verify(ctx)
	cancel()
	if e != nil {
		return e
	}
	candidate.BotID = botID
	first := old.OwnerID == 0
	e = m.operation(func() error {
		temp, e := os.MkdirTemp(m.Root, ".login-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(temp)
		session := filepath.Join(temp, "session.json")
		if field != "api_id" && field != "api_hash" && field != "account" && regular(filepath.Join(m.data(), "session.json")) {
			if e = copyFile(filepath.Join(m.data(), "session.json"), session, 0600); e != nil {
				return e
			}
		}
		loginCtx, cancelLogin := context.WithTimeout(setupCtx, 10*time.Minute)
		defer cancelLogin()
		id, name, e := LoginTelegram(loginCtx, candidate, session, func(text string, secret bool) (string, error) {
			return promptContext(loginCtx, text, secret)
		})
		if e != nil {
			return e
		}
		changed := old.OwnerID > 0 && old.OwnerID != id
		if changed {
			yes, e := confirmContext(loginCtx, fmt.Sprintf("更换为 %s（ID %d），停用旧群组规则？", name, id))
			if e != nil {
				return e
			}
			if !yes {
				return errCancelled
			}
		}
		candidate.OwnerID = id
		if e = loginCtx.Err(); e != nil {
			return e
		}
		if e = validateConfig(candidate, true); e != nil {
			return e
		}
		if e = copyFile(session, filepath.Join(m.data(), "session.json"), 0600); e != nil {
			return e
		}
		if e = writeJSON(path, candidate, 0600); e != nil {
			return e
		}
		store, e := OpenStore(filepath.Join(m.data(), "state.db"))
		if e != nil {
			return e
		}
		defer store.Close()
		if changed {
			return store.DisableAll()
		}
		return nil
	}, first, first)
	if e != nil {
		return e
	}
	os.Remove(filepath.Join(m.data(), "setup-draft.json"))
	fmt.Printf("配置已保存。请打开通知 Bot @%s，发送 /start。\n", user)
	shortcutHint()
	return nil
}
func downloadFile(address, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	request, e := http.NewRequestWithContext(ctx, "GET", address, nil)
	if e != nil {
		return e
	}
	request.Header.Set("User-Agent", project)
	response, e := http.DefaultClient.Do(request)
	if e != nil {
		return errors.New("下载失败，请检查网络连接")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败：HTTP %d", response.StatusCode)
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(f, io.LimitReader(response.Body, 512<<20))
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	return e
}
func compareVersions(a, b string) int {
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.ParseUint(aa[i], 10, 64)
		y, _ := strconv.ParseUint(bb[i], 10, 64)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}
func (m Manager) Update(wanted, binary string) error {
	temp, e := os.MkdirTemp(m.Root, ".update-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(temp)
	expectedVersion := ""
	if binary == "" {
		endpoint := "https://api.github.com/repos/" + repository + "/releases/latest"
		if wanted != "" {
			wanted = strings.TrimPrefix(wanted, "v")
			if !versionPattern.MatchString(wanted) {
				return errors.New("版本应为 1.0.0 形式")
			}
			endpoint = "https://api.github.com/repos/" + repository + "/releases/tags/v" + wanted
		}
		jsonPath := filepath.Join(temp, "release.json")
		if e = downloadFile(endpoint, jsonPath); e != nil {
			return e
		}
		var release struct {
			Tag    string `json:"tag_name"`
			Body   string `json:"body"`
			Assets []struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			} `json:"assets"`
		}
		raw, e := os.ReadFile(jsonPath)
		if e != nil {
			return e
		}
		if e = json.Unmarshal(raw, &release); e != nil {
			return e
		}
		expectedVersion = strings.TrimPrefix(release.Tag, "v")
		if !versionPattern.MatchString(expectedVersion) {
			return errors.New("发行版本格式异常")
		}
		if wanted != "" && expectedVersion != wanted {
			return errors.New("发行版本与请求不符")
		}
		fmt.Printf("目标版本：%s\n%s\n", expectedVersion, release.Body)
		if compareVersions(expectedVersion, version) < 0 {
			fmt.Println("此版本早于当前程序。")
		}
		yes, e := confirm("确认更新到此版本？")
		if e != nil {
			return e
		}
		if !yes {
			return nil
		}
		arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
		name := "tgw-linux-" + arch
		assets := map[string]string{}
		for _, a := range release.Assets {
			assets[a.Name] = a.URL
		}
		if assets[name] == "" {
			return errors.New("发行文件缺失")
		}
		binary = filepath.Join(temp, name)
		if e = downloadFile(assets[name], binary); e != nil {
			return e
		}
	} else {
		binary, e = filepath.Abs(binary)
		if e != nil {
			return e
		}
		staged := filepath.Join(temp, "candidate")
		if e = copyFile(binary, staged, 0755); e != nil {
			return e
		}
		binary = staged
	}
	meta, e := validateBinary(binary)
	if e != nil {
		return e
	}
	if expectedVersion != "" && meta.Version != expectedVersion {
		return errors.New("程序版本与发行 tag 不一致")
	}
	if e = m.installProgram(binary, meta, false); e != nil {
		return e
	}
	fmt.Println("更新完成。")
	return nil
}
func (m Manager) Backup() error {
	var backup string
	e := m.operation(func() error {
		var t transaction
		b, e := os.ReadFile(m.journal())
		if e != nil {
			return e
		}
		if e = json.Unmarshal(b, &t); e != nil {
			return e
		}
		backup = t.Backup
		return nil
	}, false, false, false)
	if e == nil {
		fmt.Println("备份完成：" + backup)
	}
	return e
}
func (m Manager) Restore(name string) error {
	if name == "" {
		backups, e := m.listBackups()
		if e != nil {
			return e
		}
		if len(backups) == 0 {
			return errors.New("没有可恢复的备份，请将完整备份目录放入 " + filepath.Join(m.Root, "backups"))
		}
		labels := make([]string, len(backups))
		for i, backup := range backups {
			kind := "手动"
			if backup.meta.Automatic {
				kind = "自动"
			}
			labels[i] = fmt.Sprintf("%s · %s · v%s · %s", backup.meta.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"), kind, backup.meta.Manifest.Version, backup.name)
		}
		n, e := choose("选择备份（按创建时间从新到旧）", labels)
		if e != nil {
			return e
		}
		if n == 0 {
			return nil
		}
		name = backups[n-1].name
	}
	if filepath.Base(name) != name || name == "." || name == ".." {
		return errors.New("备份名称无效")
	}
	path := filepath.Join(m.Root, "backups", name)
	var meta backupMeta
	b, e := os.ReadFile(filepath.Join(path, "meta.json"))
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &meta); e != nil {
		return e
	}
	if e = validateBackupMeta(meta); e != nil {
		return e
	}
	if meta.Slot == "" {
		return errors.New("首次安装前备份只用于故障恢复")
	}
	yes, e := confirm("恢复此备份中的程序、凭据和规则？")
	if e != nil {
		return e
	}
	if !yes {
		return nil
	}
	return m.operation(func() error {
		if e := m.restoreSnapshot(path); e != nil {
			return e
		}
		return runCandidate(filepath.Join(m.Root, "current", "tgw"), "--root", m.Root, "migrate")
	}, false, false)
}
func (m Manager) Rollback() error {
	var r map[string]string
	b, e := os.ReadFile(filepath.Join(m.Root, "last-change.json"))
	if e != nil {
		return errors.New("没有可回滚的维护操作")
	}
	if e = json.Unmarshal(b, &r); e != nil {
		return e
	}
	if r["backup_id"] == "" {
		return errors.New("回滚记录缺少备份标识，请从恢复备份中选择")
	}
	backups, e := m.listBackups()
	if e != nil {
		return e
	}
	name := ""
	for _, backup := range backups {
		if backup.meta.ID == r["backup_id"] {
			if name != "" {
				return errors.New("发现多个相同标识的备份，请从恢复备份中选择")
			}
			name = backup.name
		}
	}
	if name == "" {
		return errors.New("回滚备份已被删除或移出备份目录，请从恢复备份中选择其他备份")
	}
	return m.Restore(name)
}
func (m Manager) Uninstall() (bool, error) {
	if e := validateServiceOwnership(m.Root); e != nil {
		return false, e
	}
	var keepData bool
	for {
		answer, e := prompt("卸载程序，是否保留数据和备份？[y/N]：", false)
		if e != nil {
			return false, e
		}
		switch strings.ToLower(answer) {
		case "y":
			keepData = true
		case "", "n":
			keepData = false
		default:
			fmt.Println("请输入 y 或 n；回车默认不保留数据。")
			continue
		}
		break
	}
	if e := stopService(); e != nil {
		return false, e
	}
	if e := disableService(); e != nil {
		return false, e
	}
	release, e := acquireLock(filepath.Join(m.Root, "instance.lock"))
	if e != nil {
		return false, e
	}
	defer release()
	if e = os.Remove(servicePath()); e != nil && !os.IsNotExist(e) {
		return false, e
	}
	if e = os.Remove(shortcutPath); e != nil && !os.IsNotExist(e) {
		return false, e
	}
	if e = reloadService(); e != nil {
		return false, e
	}
	if e = os.Remove(filepath.Join(m.Root, "current")); e != nil && !os.IsNotExist(e) {
		return false, e
	}
	if e = os.RemoveAll(filepath.Join(m.Root, "releases")); e != nil {
		return false, e
	}
	if e = os.Remove(filepath.Join(m.Root, "installed")); e != nil && !os.IsNotExist(e) {
		return false, e
	}
	if !keepData {
		for _, name := range []string{"data", "backups", "logs", ".service-state.json", "transaction.json", "last-change.json", ".data.restore", ".data.discard", "instance.lock", "maintenance.lock", ".owner"} {
			if e = os.RemoveAll(filepath.Join(m.Root, name)); e != nil {
				return false, e
			}
		}
		if e = os.Remove(m.Root); e != nil {
			fmt.Println("已清理项目数据，安装目录仍有其他文件，予以保留。")
		}
	}
	if keepData {
		fmt.Println("卸载完成，数据和备份已保留。")
	} else {
		fmt.Println("卸载完成，数据和备份已删除。")
	}
	return true, nil
}

func (m Manager) ensureOwnedRoot() error {
	marker := filepath.Join(m.Root, ".owner")
	if b, e := os.ReadFile(marker); e == nil {
		if string(b) != project+"\n" {
			return errors.New("安装目录归属不符")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	entries, e := os.ReadDir(m.Root)
	if e == nil && len(entries) > 0 {
		return errors.New("安装目录已有其他内容，请使用空的专用目录")
	}
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if e = os.MkdirAll(m.Root, 0700); e != nil {
		return e
	}
	if e = os.Chmod(m.Root, 0700); e != nil {
		return e
	}
	return atomicWrite(marker, []byte(project+"\n"), 0600)
}
