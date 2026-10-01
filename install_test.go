package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testEnv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func TestConfigFileSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rtelegram.conf")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("# comment\n\ntoken = 123:FILE\nmasters = 7, 8\nurl = /run/file.sock\nadd-stopped = true\nlow-disk = 1G\n")

	cfg, err := parseConfig([]string{"-config", path}, testEnv(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.token != "123:FILE" || len(cfg.masters.ids) != 2 || cfg.rtorrentAddress != "/run/file.sock" || !cfg.addStopped || cfg.lowDisk != 1<<30 {
		t.Fatalf("config from file = %+v", cfg)
	}
	if cfg.rewriteConfig {
		t.Fatal("settings only from the config file asked for it to be rewritten")
	}
	if cfg, err := parseConfig([]string{"-config", path, "-no-live"}, testEnv(nil), io.Discard); err != nil || !cfg.rewriteConfig || cfg.settings["no-live"] != "true" {
		t.Fatalf("a flag beside the config file: rewrite %v, settings %v, %v", cfg.rewriteConfig, cfg.settings, err)
	}

	// Flags beat the file, which beats the environment.
	cfg, err = parseConfig([]string{"-config", path, "-url", "/run/flag.sock"},
		testEnv(map[string]string{"RT_URL": "/run/env.sock", "RT_TOKEN": "123:ENV", "RT_INDEXER_KEY": "key"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.rtorrentAddress != "/run/flag.sock" || cfg.token != "123:FILE" || cfg.indexerKey != "key" || !cfg.rewriteConfig {
		t.Fatalf("precedence: url %q, token %q, indexer key %q, rewrite %v", cfg.rtorrentAddress, cfg.token, cfg.indexerKey, cfg.rewriteConfig)
	}
	if cfg.settings["url"] != "/run/flag.sock" || cfg.settings["token"] != "123:FILE" || cfg.settings["indexer-key"] != "key" ||
		cfg.settings["masters"] != "7, 8" || cfg.settings["low-disk"] != "1G" {
		t.Fatalf("settings = %v", cfg.settings)
	}
	if _, ok := cfg.settings["config"]; ok {
		t.Fatalf("settings include -config: %v", cfg.settings)
	}

	for text, want := range map[string]string{
		"token = 1:A\nnonsense\n": "rtelegram.conf:2: expected a setting",
		"bogus = 1\n":             `"bogus" is not a setting`,
		"install = true\n":        `"install" is not a setting`,
		"token = 1:A\nmasters = 7\nwatch-interval = soon\n": "rtelegram.conf:3: watch-interval",
	} {
		write(text)
		if _, err := parseConfig([]string{"-config", path}, testEnv(nil), io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("config %q: error %v, want %q", text, err, want)
		}
	}
	if _, err := parseConfig([]string{"-config", filepath.Join(dir, "missing.conf")}, testEnv(nil), io.Discard); err == nil {
		t.Error("a missing config file was accepted")
	}
	// -uninstall needs no settings.
	if cfg, err := parseConfig([]string{"-uninstall"}, testEnv(nil), io.Discard); err != nil || !cfg.uninstall {
		t.Errorf("-uninstall = %+v, %v", cfg, err)
	}
	if _, err := parseConfig([]string{"-install", "-uninstall"}, testEnv(nil), io.Discard); err == nil {
		t.Error("-install with -uninstall was accepted")
	}
}

type testInstall struct {
	host     serviceHost
	commands []string
	out      bytes.Buffer
	// failing makes commands that start with it fail.
	failing []string
}

func newTestInstall(t *testing.T, goos string, admin bool) *testInstall {
	t.Helper()
	prefix := t.TempDir()
	home := filepath.Join(prefix, "home", "alice")
	ti := &testInstall{}
	ti.host = serviceHost{
		goos: goos, stdout: &ti.out, admin: admin, uid: 501,
		account:    serviceAccount{name: "alice", home: home, configDir: userConfigDir(goos, home), uid: -1, gid: -1},
		executable: "/opt/rtelegram/rtelegram",
		prefix:     prefix,
		command: func(name string, args ...string) error {
			command := name + " " + strings.Join(args, " ")
			ti.commands = append(ti.commands, command)
			for _, failing := range ti.failing {
				if strings.HasPrefix(command, failing) {
					return errors.New("failed")
				}
			}
			return nil
		},
		check: func(context.Context, config, io.Writer) error { return nil },
	}
	if err := os.MkdirAll(filepath.Join(prefix, "run", "systemd", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	return ti
}

func installConfig(t *testing.T, args ...string) config {
	t.Helper()
	cfg, err := parseConfig(append([]string{"-install"}, args...),
		testEnv(map[string]string{"RT_TOKEN": "123:SECRET", "RT_MASTERS": "7", "RT_URL": "/run/rtorrent.sock"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInstallSystemdUserService(t *testing.T) {
	ti := newTestInstall(t, "linux", false)
	dataRoot := t.TempDir()
	if err := installService(context.Background(), installConfig(t, "-data-root", dataRoot), ti.host); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(ti.host.account.home, ".config")
	configPath := filepath.Join(configDir, "rtelegram", "rtelegram.conf")
	settings := readFile(t, configPath)
	for _, want := range []string{"token = 123:SECRET\n", "masters = 7\n", "url = /run/rtorrent.sock\n", "data-root = " + dataRoot + "\n",
		"state = " + filepath.Join(configDir, "rtelegram", "state.json") + "\n"} {
		if !strings.Contains(settings, want) {
			t.Errorf("config file lacks %q:\n%s", want, settings)
		}
	}
	if info, err := os.Stat(configPath); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("config file mode = %v, %v", info.Mode(), err)
	}
	unit := readFile(t, filepath.Join(configDir, "systemd", "user", "rtelegram.service"))
	for _, want := range []string{`ExecStart="/opt/rtelegram/rtelegram" -config ` + systemdArg(configPath), "Restart=on-failure", "WantedBy=default.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "User=") || strings.Contains(unit, "network-online") {
		t.Errorf("a user unit names a user or a system target:\n%s", unit)
	}
	if got := strings.Join(ti.commands, "; "); got != "systemctl --user daemon-reload; systemctl --user enable rtelegram.service; "+
		"systemctl --user restart rtelegram.service; loginctl enable-linger alice" {
		t.Fatalf("commands = %s", got)
	}
	if !strings.Contains(ti.out.String(), "starts at boot") || !strings.Contains(ti.out.String(), "journalctl --user -u rtelegram") {
		t.Errorf("output = %s", ti.out.String())
	}

	// The saved file is enough to run the bot.
	cfg, err := parseConfig([]string{"-config", configPath}, testEnv(nil), io.Discard)
	if err != nil || cfg.token != "123:SECRET" || cfg.dataRoot != dataRoot || cfg.statePath != filepath.Join(configDir, "rtelegram", "state.json") {
		t.Fatalf("running from the config file: %+v, %v", cfg, err)
	}

	ti.commands = nil
	if err := uninstallService(ti.host); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ti.commands, "; "); got != "systemctl --user disable --now rtelegram.service; systemctl --user daemon-reload" {
		t.Fatalf("uninstall commands = %s", got)
	}
	if _, err := os.Stat(filepath.Join(configDir, "systemd", "user", "rtelegram.service")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit still exists: %v", err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("uninstall removed the settings: %v", err)
	}
	if err := uninstallService(ti.host); err == nil || !strings.Contains(err.Error(), "no rtelegram service") {
		t.Fatalf("second uninstall = %v", err)
	}
}

func TestInstallSystemdWithoutLingerStartsAtLogin(t *testing.T) {
	ti := newTestInstall(t, "linux", false)
	ti.failing = []string{"loginctl"}
	if err := installService(context.Background(), installConfig(t), ti.host); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ti.out.String(), "sudo loginctl enable-linger alice") {
		t.Fatalf("output = %s", ti.out.String())
	}
}

func TestInstallSystemdSystemService(t *testing.T) {
	ti := newTestInstall(t, "linux", true)
	if err := installService(context.Background(), installConfig(t), ti.host); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(ti.host.prefix, "etc", "systemd", "system", "rtelegram.service")
	unit := readFile(t, unitPath)
	for _, want := range []string{"User=alice\n", "After=network-online.target", "WantedBy=multi-user.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("system unit lacks %q:\n%s", want, unit)
		}
	}
	if got := strings.Join(ti.commands, "; "); got != "systemctl daemon-reload; systemctl enable rtelegram.service; systemctl restart rtelegram.service" {
		t.Fatalf("commands = %s", got)
	}
	// Without root, uninstall points at sudo.
	user := ti.host
	user.admin = false
	if err := uninstallService(user); err == nil || !strings.Contains(err.Error(), "sudo rtelegram -uninstall") {
		t.Fatalf("uninstall without root = %v", err)
	}
	if err := uninstallService(ti.host); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit still exists: %v", err)
	}
}

func TestInstallChecksBeforeWritingAnything(t *testing.T) {
	ti := newTestInstall(t, "linux", false)
	ti.host.check = func(context.Context, config, io.Writer) error { return errors.New("rTorrent: connection refused") }
	if err := installService(context.Background(), installConfig(t), ti.host); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("install = %v", err)
	}
	if _, err := os.Stat(filepath.Join(ti.host.account.home, ".config")); !errors.Is(err, os.ErrNotExist) || len(ti.commands) != 0 {
		t.Fatalf("a failed check left files (%v) or ran %v", err, ti.commands)
	}

	ti = newTestInstall(t, "linux", false)
	if err := os.RemoveAll(filepath.Join(ti.host.prefix, "run", "systemd")); err != nil {
		t.Fatal(err)
	}
	if err := installService(context.Background(), installConfig(t), ti.host); err == nil || !strings.Contains(err.Error(), "systemd") {
		t.Fatalf("install without systemd = %v", err)
	}
	ti.host.goos = "plan9"
	if err := installService(context.Background(), installConfig(t), ti.host); err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("install on plan9 = %v", err)
	}
}

func TestInstallKeepsAHandWrittenConfigFile(t *testing.T) {
	ti := newTestInstall(t, "linux", false)
	path := filepath.Join(t.TempDir(), "my.conf")
	text := "# mine\ntoken = 123:SECRET\nmasters = 7\nstate = /var/lib/rtelegram/state.json\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig([]string{"-install", "-config", path}, testEnv(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := installService(context.Background(), cfg, ti.host); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != text {
		t.Fatalf("install rewrote the config file:\n%s", got)
	}
	unit := readFile(t, filepath.Join(ti.host.account.configDir, "systemd", "user", "rtelegram.service"))
	if !strings.Contains(unit, "-config "+systemdArg(path)) {
		t.Fatalf("unit does not use the given config file:\n%s", unit)
	}
}

func TestSystemdArgEscapesSpecifiersAndQuotes(t *testing.T) {
	if got := systemdArg(`/opt/my "bots"/50%$HOME\x`); got != `"/opt/my \"bots\"/50%%$$HOME\\x"` {
		t.Fatalf("systemdArg = %s", got)
	}
}

func TestInstallLaunchd(t *testing.T) {
	ti := newTestInstall(t, "darwin", false)
	// Over SSH there is no desktop session to load into.
	ti.failing = []string{"launchctl bootstrap gui/"}
	if err := installService(context.Background(), installConfig(t), ti.host); err != nil {
		t.Fatal(err)
	}
	home := ti.host.account.home
	configPath := filepath.Join(home, "Library", "Application Support", "rtelegram", "rtelegram.conf")
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.github.pyed.rtelegram.plist")
	plist := readFile(t, plistPath)
	for _, want := range []string{"<string>/opt/rtelegram/rtelegram</string>", "<string>" + xmlEscape(configPath) + "</string>",
		"<key>RunAtLoad</key>", filepath.Join(home, "Library", "Logs", "rtelegram.log")} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "UserName") {
		t.Errorf("an agent names a user:\n%s", plist)
	}
	if got := strings.Join(ti.commands, "; "); got != "launchctl bootout gui/501/com.github.pyed.rtelegram; launchctl bootstrap gui/501 "+plistPath+
		"; launchctl bootout user/501/com.github.pyed.rtelegram; launchctl bootstrap user/501 "+plistPath {
		t.Fatalf("commands = %s", got)
	}
	if !strings.Contains(ti.out.String(), "launchctl kickstart -k user/501/com.github.pyed.rtelegram") {
		t.Errorf("output = %s", ti.out.String())
	}
	if err := uninstallService(ti.host); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist still exists: %v", err)
	}

	daemon := newTestInstall(t, "darwin", true)
	if err := installService(context.Background(), installConfig(t), daemon.host); err != nil {
		t.Fatal(err)
	}
	plist = readFile(t, filepath.Join(daemon.host.prefix, "Library", "LaunchDaemons", "com.github.pyed.rtelegram.plist"))
	if !strings.Contains(plist, "<key>UserName</key>\n\t<string>alice</string>") {
		t.Fatalf("daemon plist:\n%s", plist)
	}
	if got := daemon.commands[len(daemon.commands)-1]; !strings.HasPrefix(got, "launchctl bootstrap system ") {
		t.Fatalf("daemon loaded with %s", got)
	}
}

func TestInstallWindowsService(t *testing.T) {
	ti := newTestInstall(t, "windows", true)
	var installed []string
	ti.host.installWindows = func(exe string, args []string) error {
		installed = append([]string{exe}, args...)
		return nil
	}
	if err := installService(context.Background(), installConfig(t), ti.host); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(ti.host.account.configDir, "rtelegram", "rtelegram.conf")
	if strings.Join(installed, " ") != "/opt/rtelegram/rtelegram -config "+configPath {
		t.Fatalf("installed %v", installed)
	}
	if !strings.Contains(ti.out.String(), filepath.Join(ti.host.account.configDir, "rtelegram", "rtelegram.log")) {
		t.Errorf("output = %s", ti.out.String())
	}
}
