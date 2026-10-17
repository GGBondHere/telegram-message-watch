package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

var errCancelled = errors.New("已取消")
var inputFile *os.File
var inputReader *bufio.Reader
var terminalInput *contextInputReader

// Poll before every underlying read, including partial UTF-8/escape sequences.
// No abandoned reader goroutine may consume input after a timeout or cancellation.
type contextInputReader struct {
	file *os.File
	ctx  context.Context
}

func (r *contextInputReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		fds := []unix.PollFd{{Fd: int32(r.file.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		return r.file.Read(p)
	}
}

func shortcutHint() {
	command := "tgw"
	if term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("TERM") != "dumb" && os.Getenv("NO_COLOR") == "" {
		command = "\033[1;36mtgw\033[0m"
	}
	fmt.Printf("以后输入 %s 即可打开管理面板。\n", command)
}

func requireConfiguration(m Manager) error {
	if _, e := readConfig(filepath.Join(m.data(), "config.json"), true); e != nil {
		return &ConfigError{"首次配置未完成或配置不可用，请运行 tgw，选择「1. 首次配置」；也可从「4. 更新与备份」恢复已有备份。"}
	}
	return nil
}

func readyToStart(m Manager) error {
	if e := requireConfiguration(m); e != nil {
		return e
	}
	if !regular(filepath.Join(m.data(), "session.json")) {
		return &ConfigError{"登录信息缺失，请运行 tgw，选择「1. 首次配置」修复。"}
	}
	return nil
}

func readMaskedInput() (value string, result error) {
	fd := int(inputFile.Fd())
	state, e := term.MakeRaw(fd)
	if e != nil {
		return "", e
	}
	fmt.Print("\033[?2004h")
	defer func() {
		fmt.Print("\033[?2004l")
		result = errors.Join(result, term.Restore(fd, state))
		fmt.Println()
	}()
	var input []rune
	pasting := false
	for {
		ch, _, e := inputReader.ReadRune()
		if e != nil {
			return "", e
		}
		if ch == '\033' {
			prefix, _, e := inputReader.ReadRune()
			if e != nil {
				return "", e
			}
			if prefix != '[' && prefix != 'O' {
				continue
			}
			sequence := []rune{prefix}
			for len(sequence) < 32 {
				next, _, e := inputReader.ReadRune()
				if e != nil {
					return "", e
				}
				sequence = append(sequence, next)
				if next >= '@' && next <= '~' {
					break
				}
			}
			switch string(sequence) {
			case "[200~":
				pasting = true
			case "[201~":
				pasting = false
			}
			continue
		}
		switch ch {
		case '\003', '\004': // Ctrl+C / Ctrl+D.
			return "", errCancelled
		case '\r', '\n':
			if !pasting {
				return string(input), nil
			}
		case '\b', '\177':
			if len(input) > 0 {
				input = input[:len(input)-1]
				fmt.Print("\b \b")
			}
			continue
		case '\025': // Ctrl+U clears the field.
			fmt.Print(strings.Repeat("\b \b", len(input)))
			input = input[:0]
			continue
		}
		if unicode.IsPrint(ch) || pasting && unicode.IsSpace(ch) {
			input = append(input, ch)
			fmt.Print("*")
		}
	}
}

func prompt(text string, secret bool) (string, error) {
	return promptContext(context.Background(), text, secret)
}

func promptContext(ctx context.Context, text string, secret bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if inputFile == nil {
		inputFile = os.Stdin
		if !term.IsTerminal(int(inputFile.Fd())) {
			f, e := os.OpenFile("/dev/tty", os.O_RDWR, 0)
			if e != nil {
				return "", errors.New("需要交互终端，请在 SSH 终端中运行")
			}
			inputFile = f
		}
	}
	if terminalInput == nil || terminalInput.file != inputFile {
		terminalInput = &contextInputReader{file: inputFile}
		inputReader = bufio.NewReader(terminalInput)
	}
	terminalInput.ctx = ctx
	fmt.Print(text)
	var value string
	if secret {
		masked, e := readMaskedInput()
		if e != nil {
			return "", e
		}
		value = masked
	} else {
		line, e := inputReader.ReadString('\n')
		if e != nil {
			return "", e
		}
		value = line
	}
	value = strings.TrimSpace(value)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if value == "/cancel" {
		return "", errCancelled
	}
	return value, nil
}
func choose(title string, items []string) (int, error) {
	return chooseContext(context.Background(), title, items, "返回")
}

func chooseContext(ctx context.Context, title string, items []string, zeroLabel string) (int, error) {
	fmt.Println("\n" + title)
	for i, item := range items {
		fmt.Printf("%d. %s\n", i+1, item)
	}
	fmt.Println("0. " + zeroLabel)
	for {
		value, e := promptContext(ctx, "请选择数字：", false)
		if e != nil {
			return 0, e
		}
		n, e := strconv.Atoi(value)
		if e == nil && n >= 0 && n <= len(items) {
			return n, nil
		}
		fmt.Println("请输入菜单中的数字。")
	}
}
func confirm(text string) (bool, error) {
	return confirmContext(context.Background(), text)
}

func confirmContext(ctx context.Context, text string) (bool, error) {
	n, e := chooseContext(ctx, text, []string{"确认", "取消"}, "返回")
	return n == 1, e
}

func printOperationError(err error) {
	prefix := "操作失败："
	if errors.Is(err, errCancelled) || errors.Is(err, context.Canceled) {
		prefix = "操作已取消："
	} else if errors.Is(err, context.DeadlineExceeded) {
		prefix = "操作超时："
	}
	fmt.Println(prefix + redact(err.Error()))
}

type options struct {
	root, command                          string
	showVersion, manifest, selfCheck, help bool
}

func parseOptions(args []string) (options, error) {
	o := options{root: "/opt/tg-watch"}
	pos := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version", "-v":
			o.showVersion = true
		case "--manifest":
			o.manifest = true
		case "--self-check":
			o.selfCheck = true
		case "--help", "-h":
			o.help = true
		case "--root":
			i++
			if i >= len(args) {
				return o, errors.New("--root 缺少参数")
			}
			o.root = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return o, fmt.Errorf("未知参数：%s", args[i])
			}
			pos = append(pos, args[i])
		}
	}
	if len(pos) > 1 {
		return o, errors.New("此命令不接受额外参数；其他操作请运行 tgw 从菜单进入")
	}
	if len(pos) > 0 {
		o.command = pos[0]
	}
	switch o.command {
	case "", "start", "stop", "restart", "logs":
	case "install", "run", "migrate", "recover", "service-pre", "wait-ready": // 安装器、服务和维护子进程使用。
	default:
		return o, fmt.Errorf("未知命令：%s；请运行 tgw 从菜单选择操作", o.command)
	}
	return o, nil
}
func selfCheck() error {
	temp, e := os.MkdirTemp("", "tgw-check-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(temp)
	store, e := OpenStore(filepath.Join(temp, "state.db"))
	if e != nil {
		return e
	}
	defer store.Close()
	if e = store.SetChat(context.Background(), 1, "依赖检查"); e != nil {
		return e
	}
	if _, _, e = store.AddRules(context.Background(), 1, []Rule{{Keyword: "检查"}}, 0); e != nil {
		return e
	}
	fmt.Println("原生程序与 SQLite 可用。")
	return nil
}
func manageService(m Manager, command string) error {
	switch command {
	case "start":
		if e := readyToStart(m); e != nil {
			return e
		}
		return startService(m.Root)
	case "stop":
		return stopService()
	case "restart":
		if e := readyToStart(m); e != nil {
			return e
		}
		if e := stopService(); e != nil {
			return e
		}
		return startService(m.Root)
	}
	return errors.New("无效操作")
}
func updateProgram(m Manager) error {
	target, e := prompt("更新目标（回车使用最新 Release；也可填版本号或本地程序绝对路径；/cancel 返回）：", false)
	if e != nil {
		return e
	}
	if filepath.IsAbs(target) {
		return m.Update("", target)
	}
	return m.Update(target, "")
}
func configurationMenu(m Manager) error {
	if e := requireConfiguration(m); e != nil {
		return e
	}
	for {
		n, e := choose("修改初始化配置", []string{"API_ID", "API_HASH", "Bot Token", "个人账号重新登录"})
		if e != nil {
			return e
		}
		if n == 0 {
			return nil
		}
		if e = m.withMaintenance(func() error { return m.Setup([]string{"api_id", "api_hash", "bot_token", "account"}[n-1]) }); e != nil {
			printOperationError(e)
		}
	}
}
func mainMenu(m Manager) error {
	for {
		installed := m.installed()
		status := "未安装"
		if installed {
			status = "待首次配置"
			if regular(filepath.Join(m.data(), "config.json")) {
				status = "配置需修复"
			}
			if readyToStart(m) == nil {
				status = "服务停止"
			}
			if serviceActive() {
				status = "服务运行"
			}
		}
		n, e := chooseContext(context.Background(), "Telegram Message Watch v"+version+" · "+status, []string{"首次配置", "修改配置", "服务管理", "更新与备份", "卸载"}, "退出")
		if e != nil {
			return e
		}
		if n == 0 {
			return nil
		}
		if !installed {
			fmt.Println("尚未安装，请先运行 README 中的安装命令，再运行 tgw。")
			continue
		}
		switch n {
		case 1:
			e = m.withMaintenance(func() error { return m.Setup("") })
		case 2:
			e = configurationMenu(m)
		case 3:
			var sub int
			sub, e = choose("服务管理", []string{"启动", "停止", "重启", "查看日志"})
			if e == nil && sub > 0 {
				if sub == 4 {
					e = showServiceLogs(m.Root)
				} else {
					e = m.withMaintenance(func() error { return manageService(m, []string{"start", "stop", "restart"}[sub-1]) })
				}
			}
		case 4:
			var sub int
			sub, e = choose("更新与备份", []string{"更新程序", "创建备份", "恢复备份", "回滚上一份程序和数据"})
			if e == nil && sub > 0 {
				e = m.withMaintenance(func() error {
					switch sub {
					case 1:
						return updateProgram(m)
					case 2:
						return m.Backup()
					case 3:
						return m.Restore("")
					default:
						return m.Rollback()
					}
				})
			}
		case 5:
			removed := false
			e = m.withMaintenance(func() error { var err error; removed, err = m.Uninstall(); return err })
			if e == nil && removed {
				return nil
			}
		}
		if e != nil {
			printOperationError(e)
		}
	}
}
func entry() (result error) {
	o, e := parseOptions(os.Args[1:])
	if e != nil {
		return e
	}
	if o.help {
		fmt.Println("Telegram Message Watch\n用法：tgw [start|stop|restart|logs]\n直接运行 tgw 打开管理菜单，完成首次配置、修改配置、更新、备份、恢复、回滚或卸载。\n  start     启动服务\n  stop      停止服务\n  restart   重启服务\n  logs      查看服务日志\n  --version 查看版本\n  --help    查看帮助\n交互输入 /cancel 或按 Ctrl+C 取消。")
		return nil
	}
	if o.showVersion {
		fmt.Println(version)
		return nil
	}
	if o.manifest {
		return json.NewEncoder(os.Stdout).Encode(currentManifest())
	}
	if o.selfCheck {
		return selfCheck()
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("请在 Linux 上以 root 身份运行")
	}
	syscall.Umask(0077)
	root, e := safeRoot(o.root)
	if e != nil {
		return e
	}
	m := Manager{Root: root}
	switch o.command {
	case "recover":
		if !exists(m.journal()) {
			return nil
		}
		var t transaction
		b, e := os.ReadFile(m.journal())
		if e != nil {
			return e
		}
		if e = json.Unmarshal(b, &t); e != nil {
			return e
		}
		if ownerAlive(t) {
			if t.Phase == "validating" {
				return nil
			}
			return errors.New("维护正在进行")
		}
		release, e := acquireLock(filepath.Join(root, "maintenance.lock"))
		if e != nil {
			return e
		}
		defer release()
		return m.recover(serviceStartup())
	case "service-pre":
		return prepareOpenRCService(m)
	case "wait-ready":
		return waitServiceReady(root)
	case "migrate":
		store, e := OpenStore(filepath.Join(m.data(), "state.db"))
		if e != nil {
			return e
		}
		return store.Close()
	case "run":
		if exists(m.journal()) {
			var t transaction
			b, err := os.ReadFile(m.journal())
			if err != nil || json.Unmarshal(b, &t) != nil || t.Phase != "validating" || !ownerAlive(t) {
				return &ConfigError{"维护事务未完成，请运行 tgw restart，恢复维护事务并重启服务"}
			}
		}
		release, e := acquireLock(filepath.Join(root, "instance.lock"))
		if e != nil {
			return e
		}
		defer release()
		closeLog, e := serviceLogging(root)
		if e != nil {
			return e
		}
		defer func() { closeLog(result) }()
		if openrcRuntime() {
			if e = writeServiceState(root, false, nil); e != nil {
				return e
			}
			defer func() {
				result = errors.Join(result, writeServiceState(root, false, result))
			}()
		}
		cfg, e := readConfig(filepath.Join(m.data(), "config.json"), true)
		if e != nil {
			return e
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return RunRuntime(ctx, cfg, m.data())
	case "logs":
		return showServiceLogs(m.Root)
	case "":
		return mainMenu(m)
	case "install":
		return m.withMaintenance(m.Install)
	case "start", "stop", "restart":
		if !m.installed() {
			return errors.New("尚未安装，请先运行安装引导")
		}
		return m.withMaintenance(func() error { return manageService(m, o.command) })
	}
	return errors.New("无效操作")
}
func main() {
	e := entry()
	if e == nil || errors.Is(e, context.Canceled) {
		return
	}
	if errors.Is(e, errCancelled) {
		fmt.Println("已取消。")
		os.Exit(130)
	}
	fmt.Fprintln(os.Stderr, redact(e.Error()))
	var configError *ConfigError
	if errors.As(e, &configError) {
		stopOpenRCSupervisor()
		os.Exit(78)
	}
	os.Exit(1)
}
