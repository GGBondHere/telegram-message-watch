package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Isolate all service paths and commands: this test is safe on a developer's
// Linux host as well as in the containers used for verification.
func maintenanceFixture(t *testing.T, manager string, running bool) Manager {
	t.Helper()
	base := t.TempDir()
	previousManager, previousUnit, previousInit, previousShortcut, previousLevels := serviceManager, systemdUnitPath, openrcInitPath, shortcutPath, openrcRunlevelsPath
	serviceManager = manager
	systemdUnitPath, openrcInitPath = filepath.Join(base, "unit"), filepath.Join(base, "init")
	shortcutPath, openrcRunlevelsPath = filepath.Join(base, "shortcut"), filepath.Join(base, "runlevels")
	t.Cleanup(func() {
		serviceManager, systemdUnitPath, openrcInitPath, shortcutPath, openrcRunlevelsPath = previousManager, previousUnit, previousInit, previousShortcut, previousLevels
	})
	m := Manager{Root: filepath.Join(base, "root")}
	if err := os.MkdirAll(m.data(), 0700); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{filepath.Join(m.data(), "value"): "before", filepath.Join(m.Root, "installed"): project, servicePath(): "test service"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, slot := range []string{"1.0.0-000000000001", "1.0.0-000000000002"} {
		dir := filepath.Join(m.Root, "releases", slot)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tgw"), []byte(slot), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "manifest.json"), currentManifest(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.switchSlot("1.0.0-000000000001"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(m.Root, "current", "tgw"), shortcutPath); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "state")
	bin := filepath.Join(base, "bin")
	for _, dir := range []string{state, bin, filepath.Join(openrcRunlevelsPath, "default")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if running {
		if err := os.WriteFile(filepath.Join(state, "running"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if manager == "openrc" {
		if err := os.Symlink(openrcInitPath, filepath.Join(openrcRunlevelsPath, "default", openrcName)); err != nil {
			t.Fatal(err)
		}
		if err := writeServiceState(m.Root, true, nil); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(filepath.Join(state, "enabled"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
set -eu
tool=${0##*/}
if [ "$tool" = rc-service ]; then shift; fi
cmd=$1
case "$tool:$cmd" in
  systemctl:is-active|rc-service:status) test -f "$TGW_TEST_STATE/running" ;;
  systemctl:is-enabled) test -f "$TGW_TEST_STATE/enabled" ;;
  systemctl:stop|rc-service:stop) rm -f "$TGW_TEST_STATE/running" ;;
  systemctl:start|rc-service:start)
    if [ -f "$TGW_TEST_STATE/fail-start" ]; then rm "$TGW_TEST_STATE/fail-start"; exit 1; fi
    touch "$TGW_TEST_STATE/running" ;;
  systemctl:enable) touch "$TGW_TEST_STATE/enabled" ;;
  systemctl:disable) rm -f "$TGW_TEST_STATE/enabled" ;;
  rc-update:add) mkdir -p "$TGW_TEST_LEVELS/$3"; ln -sf "$TGW_TEST_INIT" "$TGW_TEST_LEVELS/$3/tg-watch" ;;
  rc-update:del) rm -f "$TGW_TEST_LEVELS/$3/tg-watch" ;;
  systemctl:daemon-reload|rc-update:--update) : ;;
  *) exit 2 ;;
esac
`
	for _, name := range []string{"systemctl", "rc-service", "rc-update"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TGW_TEST_STATE", state)
	t.Setenv("TGW_TEST_LEVELS", openrcRunlevelsPath)
	t.Setenv("TGW_TEST_INIT", openrcInitPath)
	return m
}

func TestMaintenanceRecoveryAndServiceState(t *testing.T) {
	for _, manager := range []string{"systemd", "openrc"} {
		for _, scenario := range []string{"change-failure", "start-failure", "cancel-stopped", "success-stopped", "interrupted"} {
			t.Run(manager+"/"+scenario, func(t *testing.T) {
				running := scenario != "cancel-stopped" && scenario != "success-stopped"
				m := maintenanceFixture(t, manager, running)
				change := func() error {
					if err := os.WriteFile(filepath.Join(m.data(), "value"), []byte("after"), 0600); err != nil {
						return err
					}
					if err := m.switchSlot("1.0.0-000000000002"); err != nil {
						return err
					}
					if scenario == "change-failure" {
						return errors.New("injected change failure")
					}
					if scenario == "cancel-stopped" {
						return context.Canceled
					}
					return nil
				}
				if scenario == "start-failure" {
					if err := os.WriteFile(filepath.Join(os.Getenv("TGW_TEST_STATE"), "fail-start"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if scenario == "interrupted" {
					backup, backupErr := m.backup(true)
					if backupErr != nil {
						t.Fatal(backupErr)
					}
					if err = change(); err != nil {
						t.Fatal(err)
					}
					tr := transaction{Backup: backup, Running: running, Enabled: true, Phase: "prepared", Runlevels: []string{"default"}}
					if err = writeJSON(m.journal(), tr, 0600); err != nil {
						t.Fatal(err)
					}
					err = m.recover(false)
				} else {
					err = m.operation(change, false, false)
				}
				if scenario == "interrupted" || scenario == "success-stopped" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("expected injected failure")
				}
				wantValue, wantSlot := "before", "1.0.0-000000000001"
				if scenario == "success-stopped" {
					wantValue, wantSlot = "after", "1.0.0-000000000002"
				}
				value, readErr := os.ReadFile(filepath.Join(m.data(), "value"))
				if readErr != nil || string(value) != wantValue || m.slot() != wantSlot {
					t.Fatalf("snapshot mismatch: %s %s %v", value, m.slot(), readErr)
				}
				if serviceActive() != running || !serviceEnabled() {
					t.Fatal("running/enabled state not preserved")
				}
				if exists(m.journal()) {
					t.Fatal("finished transaction journal retained")
				}
				release, err := acquireLock(filepath.Join(m.Root, "instance.lock"))
				if err != nil {
					t.Fatal("instance lock leaked", err)
				}
				release()
			})
		}
	}
}

func TestMaintenanceLockAcrossProcesses(t *testing.T) {
	if path := os.Getenv("TGW_TEST_LOCK_HELPER"); path != "" {
		release, err := acquireLock(path)
		if release != nil {
			release()
		}
		wantBusy := os.Getenv("TGW_TEST_LOCK_BUSY") == "1"
		if (err != nil) != wantBusy {
			t.Fatalf("lock result: busy=%t err=%v", wantBusy, err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "maintenance.lock")
	release, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	run := func(busy bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMaintenanceLockAcrossProcesses$", "-test.timeout=2s")
		flag := "0"
		if busy {
			flag = "1"
		}
		command.Env = append(os.Environ(), "TGW_TEST_LOCK_HELPER="+path, "TGW_TEST_LOCK_BUSY="+flag)
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("lock helper: %s %v", out, err)
		}
	}
	run(true)
	release()
	run(false)
}

func TestTerminalTimeoutRestoresState(t *testing.T) {
	for _, input := range []string{"", "\033[", "\003"} {
		t.Run(fmt.Sprintf("bytes-%x", []byte(input)), func(t *testing.T) {
			fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			master := os.NewFile(uintptr(fd), "ptmx")
			defer master.Close()
			if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer slave.Close()
			before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			oldFile, oldReader, oldTerminal, oldOutput := inputFile, inputReader, terminalInput, os.Stdout
			inputFile, inputReader, terminalInput, os.Stdout = slave, nil, nil, slave
			defer func() { inputFile, inputReader, terminalInput, os.Stdout = oldFile, oldReader, oldTerminal, oldOutput }()
			written := make(chan struct{})
			go func() {
				defer close(written)
				if input == "" {
					return
				}
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					state, e := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
					if e != nil {
						return
					}
					if state.Lflag&unix.ICANON == 0 {
						_, _ = master.Write([]byte(input))
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			_, err = promptContext(ctx, "secret: ", true)
			<-written
			if input == "\003" {
				if !errors.Is(err, errCancelled) {
					t.Fatalf("Ctrl+C: %v", err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout: %v", err)
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("terminal state not restored", err)
			}
		})
	}
}

func TestScopedCallbacksStayWithinTelegramLimit(t *testing.T) {
	group := WatchGroup{ChatID: -9223372036854775807, Revision: 9223372036854775807}
	menu := &botMenu{}
	for _, raw := range []string{
		"set-delete:" + groupKey(group) + ":1y2p0ij32e8e7",
		"preview:" + groupKey(group) + ":abcdefghijkl:9",
		"edit:" + groupKey(group) + ":1y2p0ij32e8e7:s",
	} {
		if data := menu.callbackData(raw); len(data) > 64 || strings.ContainsAny(data, " \r\n") {
			t.Fatalf("invalid callback: %q", data)
		}
	}
}
