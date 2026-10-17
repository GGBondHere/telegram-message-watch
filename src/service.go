package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var systemdUnitPath = "/etc/systemd/system/tg-watch.service"
var openrcInitPath = "/etc/init.d/tg-watch"
var openrcRunlevelsPath = "/etc/runlevels"

const openrcName = "tg-watch"

var shortcutPath = "/usr/local/bin/tgw"

const serviceName = "tg-watch.service"

var serviceManager = func() string {
	info, _ := platformInfo()
	if info["ID"] == "alpine" {
		return "openrc"
	}
	return "systemd"
}()

func platformInfo() (map[string]string, error) {
	b, e := os.ReadFile("/etc/os-release")
	if e != nil {
		return nil, e
	}
	info := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			info[k] = strings.Trim(v, "\"'")
		}
	}
	return info, nil
}
func servicePath() string {
	if serviceManager == "openrc" {
		return openrcInitPath
	}
	return systemdUnitPath
}
func serviceFileMode() os.FileMode {
	if serviceManager == "openrc" {
		return 0755
	}
	return 0644
}
func serviceCommand(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if e != nil {
		return string(b), fmt.Errorf("%s %s：%s：%w", name, strings.Join(args, " "), strings.TrimSpace(string(b)), e)
	}
	return strings.TrimSpace(string(b)), nil
}

func acquireLock(path string) (func(), error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("已有监听、登录或维护操作正在进行")
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}
func systemctl(args ...string) (string, error) {
	return serviceCommand("systemctl", args...)
}
func serviceActive() bool {
	if serviceManager == "openrc" {
		_, e := serviceCommand("rc-service", openrcName, "status")
		return e == nil
	}
	_, e := systemctl("is-active", "--quiet", serviceName)
	return e == nil
}
func enabledRunlevels() []string {
	var levels []string
	paths, _ := filepath.Glob(filepath.Join(openrcRunlevelsPath, "*", openrcName))
	for _, path := range paths {
		target, e := filepath.EvalSymlinks(path)
		if e == nil && target == openrcInitPath {
			levels = append(levels, filepath.Base(filepath.Dir(path)))
		}
	}
	return levels
}
func serviceEnabled() bool {
	if serviceManager == "openrc" {
		return len(enabledRunlevels()) > 0
	}
	_, e := systemctl("is-enabled", "--quiet", serviceName)
	return e == nil
}
func stopService() error {
	if !exists(servicePath()) {
		return nil
	}
	if serviceManager == "openrc" {
		_, e := serviceCommand("rc-service", openrcName, "stop")
		return e
	}
	_, e := systemctl("stop", serviceName)
	return e
}
func startService(root string) error {
	if serviceManager == "openrc" {
		_, e := serviceCommand("rc-service", openrcName, "start")
		if e != nil {
			return e
		}
		// OpenRC may return success for an already started service without
		// invoking start_post. Check the live worker's readiness in that case too.
		return waitServiceReady(root)
	}
	_, e := systemctl("start", serviceName)
	return e
}
func enableService() error {
	if serviceManager == "openrc" {
		if serviceEnabled() {
			return nil
		}
		_, e := serviceCommand("rc-update", "add", openrcName, "default")
		return e
	}
	_, e := systemctl("enable", serviceName)
	return e
}
func disableService() error {
	if serviceManager == "openrc" {
		for _, level := range enabledRunlevels() {
			if _, e := serviceCommand("rc-update", "del", openrcName, level); e != nil {
				return e
			}
		}
		return nil
	}
	_, e := systemctl("disable", serviceName)
	return e
}
func restoreEnableState(enabled bool, levels []string) error {
	if !enabled {
		return disableService()
	}
	if serviceManager != "openrc" {
		return enableService()
	}
	if len(levels) == 0 {
		return errors.New("OpenRC 自启恢复缺少运行级别记录，未修改当前自启状态")
	}
	for _, level := range levels {
		if level == "" || level == "." || level == ".." || strings.HasPrefix(level, "-") || strings.ContainsAny(level, "/\\ \t\r\n") {
			return errors.New("备份中的 OpenRC 运行级别无效")
		}
	}
	if e := disableService(); e != nil {
		return e
	}
	for _, level := range levels {
		if _, e := serviceCommand("rc-update", "add", openrcName, level); e != nil {
			return e
		}
	}
	return nil
}
func reloadService() error {
	if serviceManager == "openrc" {
		_, e := serviceCommand("rc-update", "--update")
		return e
	}
	_, e := systemctl("daemon-reload")
	return e
}
func validatePlatform() error {
	info, e := platformInfo()
	if e != nil {
		return e
	}
	parts := strings.Split(info["VERSION_ID"], ".")
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	valid := info["ID"] == "debian" && major >= 11 ||
		info["ID"] == "ubuntu" && (major > 20 || major == 20 && minor >= 4) ||
		info["ID"] == "alpine" && (major > 3 || major == 3 && minor >= 20)
	if !valid {
		return errors.New("需要 Debian 11+、Ubuntu 20.04+ 或 Alpine 3.20+")
	}
	if serviceManager == "openrc" {
		for _, command := range []string{"openrc-run", "rc-service", "rc-update", "supervise-daemon"} {
			if _, e := exec.LookPath(command); e != nil {
				return errors.New("需要已安装并启用 OpenRC 的 Alpine 系统")
			}
		}
		if !exists("/run/openrc") {
			return errors.New("OpenRC 尚未启动，请先配置系统的 OpenRC")
		}
		return nil
	}
	_, e = exec.LookPath("systemctl")
	if e != nil {
		return errors.New("需要使用 systemd 的系统")
	}
	return nil
}
func validateServiceOwnership(root string) error {
	if b, e := os.ReadFile(servicePath()); e == nil {
		valid := regular(servicePath())
		if serviceManager == "openrc" {
			for _, line := range []string{"description=\"Telegram Message Watch\"", "# tgw-root=" + root,
				"command=" + shellQuote(shortcutPath), "directory=" + shellQuote(root),
				"command_args=" + shellQuote("--root "+shellQuote(root)+" run")} {
				valid = valid && strings.Contains("\n"+string(b), "\n"+line+"\n")
			}
		} else {
			valid = valid && strings.Contains(string(b), "Description=Telegram Message Watch") &&
				strings.Contains(string(b), "\nWorkingDirectory="+root+"\n") &&
				strings.Contains(string(b), "\nExecStart="+shortcutPath+" --root "+root+" run\n")
		}
		if !valid {
			return errors.New("同名服务不属于本程序，拒绝覆盖")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if s, e := os.Lstat(shortcutPath); e == nil {
		link, err := os.Readlink(shortcutPath)
		if s.Mode()&os.ModeSymlink == 0 || err != nil || link != filepath.Join(root, "current", "tgw") {
			return errors.New("tgw 命令已被其他程序占用")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	return nil
}
func installService(root string) error {
	if e := validateServiceOwnership(root); e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(shortcutPath), 0755); e != nil {
		return e
	}
	tmp := shortcutPath + ".new"
	os.Remove(tmp)
	if e := os.Symlink(filepath.Join(root, "current", "tgw"), tmp); e != nil {
		return e
	}
	if e := os.Rename(tmp, shortcutPath); e != nil {
		return e
	}
	if !exists(servicePath()) {
		text := fmt.Sprintf("[Unit]\nDescription=Telegram Message Watch\nWants=network-online.target\nAfter=network-online.target\nStartLimitIntervalSec=300\nStartLimitBurst=5\n\n[Service]\nType=notify\nNotifyAccess=main\nUser=root\nWorkingDirectory=%s\nExecStartPre=%s --root %s recover\nExecStart=%s --root %s run\nRestart=on-failure\nRestartPreventExitStatus=78\nRestartSec=10\nTimeoutStartSec=45\nTimeoutStopSec=30\nUMask=0077\nNoNewPrivileges=true\n\n[Install]\nWantedBy=multi-user.target\n", root, shortcutPath, root, shortcutPath, root)
		if serviceManager == "openrc" {
			text = openrcScript(root)
		}
		if e := atomicWrite(servicePath(), []byte(text), serviceFileMode()); e != nil {
			return e
		}
	}
	return reloadService()
}
func notifyReady(root string) error {
	if openrcRuntime() {
		return writeServiceState(root, true, nil)
	}
	address := os.Getenv("NOTIFY_SOCKET")
	if address == "" {
		return nil
	}
	conn, e := net.Dial("unixgram", address)
	if e != nil {
		return e
	}
	defer conn.Close()
	_, e = conn.Write([]byte("READY=1\nSTATUS=监听与 Bot 已连接"))
	return e
}

type systemSnapshot struct {
	Manager        string
	Unit           []byte
	UnitExists     bool
	Shortcut       string
	ShortcutExists bool
	Installed      bool
}

func captureSystem(root string) (systemSnapshot, error) {
	s := systemSnapshot{Manager: serviceManager}
	if b, e := os.ReadFile(servicePath()); e == nil {
		s.Unit = b
		s.UnitExists = true
	} else if !os.IsNotExist(e) {
		return s, e
	}
	if l, e := os.Readlink(shortcutPath); e == nil {
		s.Shortcut = l
		s.ShortcutExists = true
	} else if !os.IsNotExist(e) {
		return s, e
	}
	s.Installed = regular(filepath.Join(root, "installed"))
	return s, nil
}
func restoreSystem(root string, s systemSnapshot) error {
	if e := validateSnapshotManager(s); e != nil {
		return e
	}
	if s.UnitExists {
		if e := atomicWrite(servicePath(), s.Unit, serviceFileMode()); e != nil {
			return e
		}
	} else {
		if e := os.Remove(servicePath()); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	if e := os.Remove(shortcutPath); e != nil && !os.IsNotExist(e) {
		return e
	}
	if s.ShortcutExists {
		if e := os.Symlink(s.Shortcut, shortcutPath); e != nil {
			return e
		}
	}
	marker := filepath.Join(root, "installed")
	if s.Installed {
		if e := atomicWrite(marker, []byte(project+"\n"), 0600); e != nil {
			return e
		}
	} else {
		if e := os.Remove(marker); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	return reloadService()
}

func validateSnapshotManager(s systemSnapshot) error {
	if s.Manager != serviceManager {
		return errors.New("备份的服务管理类型缺失或与当前系统不一致，未修改当前数据")
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
func openrcScript(root string) string {
	return fmt.Sprintf(`#!/sbin/openrc-run
description="Telegram Message Watch"
# tgw-root=%s
supervisor="supervise-daemon"
command=%s
directory=%s
command_args=%s
command_user="root:root"
pidfile="/run/tg-watch.supervisor.pid"
output_log="$directory/logs/tg-watch.log"
error_log="$output_log"
umask="0077"
respawn_delay=10
respawn_max=5
respawn_period=300
retry="TERM/30/KILL/5"
export TGW_SERVICE_MANAGER="openrc"

depend() {
    need net
    use dns
    after firewall
}
start_pre() {
    "$command" --root "$directory" recover || return 1
    "$command" --root "$directory" service-pre || return 1
}
start_post() {
    if ! "$command" --root "$directory" wait-ready; then
        supervise-daemon "${RC_SVCNAME}" --stop --pidfile "$pidfile"
        return 1
    fi
}
`, root, shellQuote(shortcutPath), shellQuote(root), shellQuote("--root "+shellQuote(root)+" run"))
}

func openrcRuntime() bool {
	return serviceManager == "openrc" && os.Getenv("TGW_SERVICE_MANAGER") == "openrc" && os.Getenv("RC_SVCNAME") == openrcName
}
func serviceStartup() bool {
	return os.Getenv("INVOCATION_ID") != "" || openrcRuntime()
}

type serviceRuntimeState struct {
	PID   int
	Start string
	Ready bool
	Error string
}

func writeServiceState(root string, ready bool, err error) error {
	state := serviceRuntimeState{PID: os.Getpid(), Start: processStart(os.Getpid()), Ready: ready}
	if err != nil && !errors.Is(err, context.Canceled) {
		state.Error = redact(err.Error())
	}
	return writeJSON(filepath.Join(root, ".service-state.json"), state, 0600)
}
func prepareOpenRCService(m Manager) error {
	if !m.installed() {
		return errors.New("程序尚未安装")
	}
	if e := readyToStart(m); e != nil {
		return e
	}
	if e := validateServiceOwnership(m.Root); e != nil {
		return e
	}
	if e := os.Remove(filepath.Join(m.Root, ".service-state.json")); e != nil && !os.IsNotExist(e) {
		return e
	}
	f, e := openServiceLog(m.Root)
	if e != nil {
		return e
	}
	return f.Close()
}

func serviceLogPath(root string) string {
	return filepath.Join(root, "logs", "tg-watch.log")
}

func openServiceLog(root string) (*os.File, error) {
	path := serviceLogPath(root)
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(dir)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() {
		return nil, errors.New("日志目录不是普通目录")
	}
	if e = os.Chmod(dir, 0700); e != nil {
		return nil, e
	}
	if info, e := os.Lstat(path); e == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("日志文件不是普通文件")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = f.Chmod(0600); e != nil {
		return nil, errors.Join(e, f.Close())
	}
	return f, nil
}

const serviceLogLimit = 5 << 20

type rotatingServiceLog struct {
	sync.Mutex
	file *os.File
	path string
}

func (w *rotatingServiceLog) Write(data []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	length := len(data)
	if length > serviceLogLimit {
		data = data[length-serviceLogLimit:]
	}
	info, e := w.file.Stat()
	if e != nil {
		return 0, e
	}
	if info.Size()+int64(len(data)) > serviceLogLimit {
		previous := make([]byte, min(info.Size(), serviceLogLimit))
		if _, e = w.file.ReadAt(previous, max(0, info.Size()-serviceLogLimit)); e != nil {
			return 0, e
		}
		if e = atomicWrite(w.path+".1", previous, 0600); e != nil {
			return 0, e
		}
		// Keep the inode: OpenRC also holds an append-only stderr descriptor.
		if e = w.file.Truncate(0); e != nil {
			return 0, e
		}
	}
	n, e := w.file.Write(data)
	if e == nil {
		return length, nil
	}
	return n, e
}

func serviceLogging(root string) (func(error), error) {
	if serviceManager != "openrc" {
		return func(error) {}, nil
	}
	f, e := openServiceLog(root)
	if e != nil {
		return nil, e
	}
	log.SetOutput(&rotatingServiceLog{file: f, path: f.Name()})
	return func(result error) {
		if result != nil && !errors.Is(result, context.Canceled) {
			logf("监听服务退出：%v", result)
		}
		log.SetOutput(os.Stderr)
		f.Close()
	}, nil
}

func waitServiceReady(root string) error {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var state serviceRuntimeState
		b, e := os.ReadFile(filepath.Join(root, ".service-state.json"))
		if e == nil && json.Unmarshal(b, &state) == nil {
			if state.Error != "" {
				return errors.New(state.Error)
			}
			if state.Ready && state.Start != "" && processStart(state.PID) == state.Start {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("45 秒内未收到监听与 Bot 的就绪通知，请运行 tgw logs")
}

// A permanent credential error must also stop the OpenRC supervisor.
// pidfd pins the actual parent process; do not signal a recycled PID.
func stopOpenRCSupervisor() {
	if !openrcRuntime() {
		return
	}
	parent := os.Getppid()
	if parent <= 1 {
		return
	}
	fd, e := unix.PidfdOpen(parent, 0)
	if e != nil {
		return // Older custom kernels still have the finite respawn limit.
	}
	defer unix.Close(fd)
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", parent))
	args := strings.Split(string(b), "\x00")
	if e == nil && len(args) > 1 && filepath.Base(args[0]) == "supervise-daemon" &&
		args[1] == openrcName && os.Getppid() == parent {
		_ = unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0)
	}
}
func showServiceLogs(root string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var cmd *exec.Cmd
	if serviceManager != "openrc" {
		cmd = exec.CommandContext(ctx, "journalctl", "-u", serviceName, "-n", "80", "--no-pager", "-o", "short-iso", "--no-hostname", "-f")
	} else {
		path := serviceLogPath(root)
		if !exists(path) {
			fmt.Println("暂无服务日志。首次配置完成后才会启动监听。")
			return nil
		}
		cmd = exec.CommandContext(ctx, "tail", "-n", "80", "-F", path)
	}
	fmt.Println("显示最近 80 行服务日志，并持续刷新。按 Ctrl+C 返回。")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	e := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	return e
}
