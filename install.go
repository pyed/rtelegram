package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	telegram "github.com/go-telegram/bot"
	"github.com/pyed/rtapi"
)

const (
	// serviceName names the service for systemd and Windows.
	serviceName = "rtelegram"
	// launchdLabel names it for launchd.
	launchdLabel = "com.github.pyed.rtelegram"
)

// serviceHost is the machine rtelegram installs itself on: who is installing,
// for whom, and how to reach the service manager. Tests replace its parts.
type serviceHost struct {
	goos   string
	stdout io.Writer
	// admin reports whether the installer runs as root, or elevated on
	// Windows, which installs a service for the whole system.
	admin bool
	// account is whom the service runs as.
	account serviceAccount
	// uid is the installer's own user ID; -1 on Windows.
	uid        int
	executable string
	// prefix goes before the absolute system paths the installer writes.
	prefix string
	// command runs a service manager command.
	command func(name string, args ...string) error
	// check tries the settings before anything is installed.
	check func(ctx context.Context, cfg config, stdout io.Writer) error
	// On Windows, these install and remove the service.
	installWindows   func(exe string, args []string) error
	uninstallWindows func() error
}

// serviceAccount is the user a service runs as.
type serviceAccount struct {
	name string
	home string
	// configDir is where the user's settings live, as os.UserConfigDir
	// reports for them.
	configDir string
	// uid and gid own the files written for the account; -1 when the
	// installer is the account, and its files are already its own.
	uid, gid int
}

func newServiceHost(stdout io.Writer) serviceHost {
	host := serviceHost{
		goos:             runtime.GOOS,
		stdout:           stdout,
		uid:              os.Getuid(),
		command:          runCommand,
		check:            checkConnections,
		installWindows:   installWindowsService,
		uninstallWindows: uninstallWindowsService,
	}
	host.admin = isAdmin()
	host.account.uid, host.account.gid = -1, -1
	if current, err := user.Current(); err == nil {
		host.account.name, host.account.home = current.Username, current.HomeDir
	}
	host.account.configDir, _ = os.UserConfigDir()
	// "sudo rtelegram -install" installs a system service that runs as the
	// user who ran sudo, with that user's settings.
	if sudoUser := os.Getenv("SUDO_USER"); host.admin && host.goos != "windows" && sudoUser != "" && sudoUser != "root" {
		if account, err := user.Lookup(sudoUser); err == nil {
			uid, uidErr := strconv.Atoi(account.Uid)
			gid, gidErr := strconv.Atoi(account.Gid)
			if uidErr == nil && gidErr == nil {
				host.account = serviceAccount{name: account.Username, home: account.HomeDir, uid: uid, gid: gid,
					configDir: userConfigDir(host.goos, account.HomeDir)}
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		host.executable = exe
	}
	return host
}

// userConfigDir is os.UserConfigDir for a user with home directory home.
func userConfigDir(goos, home string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(home, ".config")
}

func runCommand(name string, args ...string) error {
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if text := strings.TrimSpace(string(output)); text != "" {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (h serviceHost) path(name string) string {
	return filepath.Join(h.prefix, filepath.FromSlash(name))
}

func (h serviceHost) printf(format string, args ...any) {
	fmt.Fprintf(h.stdout, format+"\n", args...)
}

// checkConnections reaches Telegram and rTorrent with cfg, so that -install
// does not set up a service that cannot start.
func checkConnections(ctx context.Context, cfg config, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := telegram.New(cfg.token, telegram.WithSkipGetMe())
	if err != nil {
		return fmt.Errorf("telegram: %s", redact(cfg.token, err.Error()))
	}
	me, err := b.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram authorization: %s", redact(cfg.token, err.Error()))
	}
	rtorrent, err := rtapi.NewRtorrentContext(ctx, cfg.rtorrentAddress)
	if err != nil {
		return fmt.Errorf("rTorrent: %w", err)
	}
	fmt.Fprintf(stdout, "Checked: Telegram bot @%s, rTorrent %s at %s\n", me.Username, rtorrent.Version, redactAddress(cfg.rtorrentAddress))
	return nil
}

// installService saves cfg's settings to the config file and has the system
// start "rtelegram -config FILE" at boot, and now.
func installService(ctx context.Context, cfg config, h serviceHost) error {
	if h.executable == "" {
		return errors.New("-install: cannot find the rtelegram executable")
	}
	if strings.HasPrefix(h.executable, os.TempDir()) {
		return fmt.Errorf("-install: %s is a temporary build; install a built rtelegram and run -install from it", h.executable)
	}
	switch h.goos {
	case "linux":
		if _, err := os.Stat(h.path("/run/systemd/system")); err != nil {
			return errors.New("-install works with systemd, which this system does not run; have your init system run rtelegram -config FILE instead")
		}
	case "darwin", "windows":
	default:
		return fmt.Errorf("-install does not support %s; have your init system run rtelegram -config FILE instead", h.goos)
	}
	if h.account.configDir == "" {
		return errors.New("-install: cannot find the user's config directory")
	}

	configPath := cfg.configPath
	if configPath == "" {
		configPath = filepath.Join(h.account.configDir, "rtelegram", "rtelegram.conf")
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	settings := maps.Clone(cfg.settings)
	if settings["state"] == "" {
		// The service may run with another home directory, or none, so it is
		// told where the settings made so far are kept.
		settings["state"] = filepath.Join(h.account.configDir, "rtelegram", "state.json")
		cfg.rewriteConfig = true
	}
	text, err := configText(settings)
	if err != nil {
		return err
	}
	if err := h.check(ctx, cfg, h.stdout); err != nil {
		return err
	}
	if _, statErr := os.Stat(configPath); cfg.rewriteConfig || statErr != nil {
		if err := writePrivateFile(configPath, text, h.account.uid, h.account.gid); err != nil {
			return fmt.Errorf("save settings: %w", err)
		}
		h.printf("Saved the settings to %s", configPath)
	}

	switch h.goos {
	case "linux":
		return h.installSystemd(configPath)
	case "darwin":
		return h.installLaunchd(configPath)
	default:
		if err := h.installWindows(h.executable, []string{"-config", configPath}); err != nil {
			return err
		}
		h.printf("Installed and started the %s service, which starts with Windows.", serviceName)
		h.printf("Logs: %s", filepath.Join(filepath.Dir(configPath), "rtelegram.log"))
		h.printf("After editing %s, restart it with: Restart-Service %s", configPath, serviceName)
		h.printf("Remove it with: rtelegram -uninstall")
		return nil
	}
}

// uninstallService stops the service and removes it, keeping the settings.
func uninstallService(h serviceHost) error {
	var err error
	switch h.goos {
	case "linux":
		err = h.uninstallSystemd()
	case "darwin":
		err = h.uninstallLaunchd()
	case "windows":
		err = h.uninstallWindows()
	default:
		err = fmt.Errorf("-uninstall does not support %s", h.goos)
	}
	if err != nil {
		return err
	}
	h.printf("Removed the %s service. The settings in %s were kept; delete them if they are no longer needed.",
		serviceName, filepath.Join(h.account.configDir, "rtelegram"))
	return nil
}

// systemdArg quotes s as one argument of a systemd command line.
func systemdArg(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

// systemdUnit is the unit that runs exe with the config file. A system unit
// runs as account, after the network is up; a user unit runs as its owner.
func systemdUnit(exe, configPath string, system bool, account string) string {
	var unit strings.Builder
	unit.WriteString("# Written by rtelegram -install; remove it with rtelegram -uninstall.\n" +
		"[Unit]\nDescription=rtelegram, a Telegram bot for rTorrent\nDocumentation=https://github.com/pyed/rtelegram\n")
	if system {
		unit.WriteString("Wants=network-online.target\nAfter=network-online.target\n")
	}
	fmt.Fprintf(&unit, "\n[Service]\nExecStart=%s -config %s\nRestart=on-failure\nRestartSec=15\n", systemdArg(exe), systemdArg(configPath))
	if system && account != "" && account != "root" {
		fmt.Fprintf(&unit, "User=%s\n", account)
	}
	target := "default.target"
	if system {
		target = "multi-user.target"
	}
	fmt.Fprintf(&unit, "\n[Install]\nWantedBy=%s\n", target)
	return unit.String()
}

// systemdUnitPath is where the unit goes: the system's units for root, or the
// user's own.
func (h serviceHost) systemdUnitPath() string {
	if h.admin {
		return h.path("/etc/systemd/system/" + serviceName + ".service")
	}
	return filepath.Join(h.account.configDir, "systemd", "user", serviceName+".service")
}

func (h serviceHost) systemctl(args ...string) error {
	if !h.admin {
		args = append([]string{"--user"}, args...)
	}
	return h.command("systemctl", args...)
}

func (h serviceHost) installSystemd(configPath string) error {
	unitPath := h.systemdUnitPath()
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(systemdUnit(h.executable, configPath, h.admin, h.account.name)), 0o644); err != nil {
		return fmt.Errorf("write the systemd unit: %w", err)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", serviceName + ".service"}, {"restart", serviceName + ".service"}} {
		if err := h.systemctl(args...); err != nil {
			if !h.admin {
				return fmt.Errorf("%w\nIf systemctl --user cannot reach your user manager, log in as this user directly rather than with su, or run sudo rtelegram -install to install a system service", err)
			}
			return err
		}
	}
	scope, sudo := "--user ", ""
	if h.admin {
		scope, sudo = "", "sudo "
	}
	h.printf("Installed and started the %s service (%s).", serviceName, unitPath)
	if !h.admin {
		// A user's services start at boot only when the user lingers.
		if err := h.command("loginctl", "enable-linger", h.account.name); err != nil {
			h.printf("It starts when you log in. To start it at boot, run: sudo loginctl enable-linger %s", h.account.name)
		} else {
			h.printf("It starts at boot.")
		}
	} else {
		h.printf("It starts at boot, as %s.", cmp.Or(h.account.name, "root"))
	}
	h.printf("Logs: %sjournalctl %s-u %s -f", sudo, scope, serviceName)
	h.printf("After editing %s, restart it with: %ssystemctl %srestart %s", configPath, sudo, scope, serviceName)
	h.printf("Remove it with: %srtelegram -uninstall", sudo)
	return nil
}

func (h serviceHost) uninstallSystemd() error {
	unitPath := h.systemdUnitPath()
	if _, err := os.Stat(unitPath); err != nil {
		if !h.admin {
			if _, err := os.Stat(h.path("/etc/systemd/system/" + serviceName + ".service")); err == nil {
				return errors.New("rtelegram is installed as a system service; remove it with sudo rtelegram -uninstall")
			}
		}
		return fmt.Errorf("no rtelegram service is installed (%s does not exist)", unitPath)
	}
	disableErr := h.systemctl("disable", "--now", serviceName+".service")
	if err := os.Remove(unitPath); err != nil {
		return err
	}
	if err := h.systemctl("daemon-reload"); err != nil {
		return err
	}
	if disableErr != nil {
		h.printf("Note: %s", disableErr)
	}
	return nil
}

// xmlEscape escapes s for an XML text node.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// launchdPlist is the job that runs exe with the config file. A daemon runs
// as account; an agent runs as its owner.
func launchdPlist(exe, configPath, logPath string, daemon bool, account string) string {
	var plist strings.Builder
	plist.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by rtelegram -install; remove it with rtelegram -uninstall. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(exe) + `</string>
		<string>-config</string>
		<string>` + xmlEscape(configPath) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>15</integer>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(logPath) + `</string>
`)
	if daemon && account != "" && account != "root" {
		plist.WriteString("\t<key>UserName</key>\n\t<string>" + xmlEscape(account) + "</string>\n")
	}
	plist.WriteString("</dict>\n</plist>\n")
	return plist.String()
}

// launchdJob is where the job goes, its domain, and its log: a daemon for
// root, or an agent of the user's.
func (h serviceHost) launchdJob() (plistPath, domain, logPath string) {
	if h.admin {
		return h.path("/Library/LaunchDaemons/" + launchdLabel + ".plist"), "system", h.path("/Library/Logs/rtelegram.log")
	}
	return filepath.Join(h.account.home, "Library", "LaunchAgents", launchdLabel+".plist"),
		"gui/" + strconv.Itoa(h.uid), filepath.Join(h.account.home, "Library", "Logs", "rtelegram.log")
}

func (h serviceHost) installLaunchd(configPath string) error {
	plistPath, domain, logPath := h.launchdJob()
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plistPath, []byte(launchdPlist(h.executable, configPath, logPath, h.admin, h.account.name)), 0o644); err != nil {
		return fmt.Errorf("write the launchd job: %w", err)
	}
	// Reinstalling replaces a loaded job, so unload it first.
	h.command("launchctl", "bootout", domain+"/"+launchdLabel)
	if err := h.command("launchctl", "bootstrap", domain, plistPath); err != nil {
		if h.admin {
			return err
		}
		// Without a desktop session, as over SSH, agents load into the user's domain.
		domain = "user/" + strconv.Itoa(h.uid)
		h.command("launchctl", "bootout", domain+"/"+launchdLabel)
		if retryErr := h.command("launchctl", "bootstrap", domain, plistPath); retryErr != nil {
			return fmt.Errorf("%w; %w", err, retryErr)
		}
	}
	sudo := ""
	if h.admin {
		sudo = "sudo "
		h.printf("Installed and started %s (%s). It starts at boot, as %s.", launchdLabel, plistPath, cmp.Or(h.account.name, "root"))
	} else {
		h.printf("Installed and started %s (%s). It starts when you log in.", launchdLabel, plistPath)
	}
	h.printf("Logs: %s", logPath)
	h.printf("After editing %s, restart it with: %slaunchctl kickstart -k %s/%s", configPath, sudo, domain, launchdLabel)
	h.printf("Remove it with: %srtelegram -uninstall", sudo)
	return nil
}

func (h serviceHost) uninstallLaunchd() error {
	plistPath, domain, _ := h.launchdJob()
	if _, err := os.Stat(plistPath); err != nil {
		if !h.admin {
			if _, err := os.Stat(h.path("/Library/LaunchDaemons/" + launchdLabel + ".plist")); err == nil {
				return errors.New("rtelegram is installed for the whole system; remove it with sudo rtelegram -uninstall")
			}
		}
		return fmt.Errorf("no rtelegram service is installed (%s does not exist)", plistPath)
	}
	h.command("launchctl", "bootout", domain+"/"+launchdLabel)
	if !h.admin {
		h.command("launchctl", "bootout", "user/"+strconv.Itoa(h.uid)+"/"+launchdLabel)
	}
	return os.Remove(plistPath)
}
