package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pyed/rtapi"
)

// nonSettings are the flags that act rather than configure, so a config file
// cannot hold them and -install does not save them.
var nonSettings = map[string]bool{"config": true, "install": true, "uninstall": true, "version": true}

// applyConfigFile sets the flags named in the config file at path, except
// those already given on the command line. Each line is a flag name and its
// value, as "url = /run/rtorrent.sock"; blank lines and lines starting with #
// are ignored.
func applyConfigFile(fs *flag.FlagSet, path string) error {
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("-config: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, value, ok := strings.Cut(text, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" {
			return fmt.Errorf("%s:%d: expected a setting, such as url = /run/rtorrent.sock", path, line)
		}
		if fs.Lookup(name) == nil || nonSettings[name] {
			return fmt.Errorf("%s:%d: %q is not a setting; see rtelegram -help", path, line, name)
		}
		if given[name] {
			continue
		}
		if err := fs.Set(name, value); err != nil {
			return fmt.Errorf("%s:%d: %s: %w", path, line, name, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("-config: %w", err)
	}
	return nil
}

// configText renders settings as a config file.
func configText(settings map[string]string) (string, error) {
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	slices.Sort(names)
	var text strings.Builder
	text.WriteString("# rtelegram settings: a flag name and its value per line (see rtelegram -help).\n" +
		"# This file holds the bot token, so only its owner may read it.\n")
	for _, name := range names {
		value := settings[name]
		if strings.ContainsAny(value, "\r\n") {
			return "", fmt.Errorf("the %s setting cannot be saved: it spans lines", name)
		}
		fmt.Fprintf(&text, "%s = %s\n", name, value)
	}
	return text.String(), nil
}

// writePrivateFile replaces the file at path with content readable only by
// its owner, creating its directory if needed. When uid is not -1, the file
// and the directories it creates are given to uid and gid.
func writePrivateFile(path, content string, uid, gid int) error {
	dir := filepath.Dir(path)
	if err := mkdirOwned(dir, uid, gid); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".rtelegram-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, err = temp.WriteString(content)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(temp.Name(), 0o600)
	}
	if err == nil && uid != -1 {
		err = os.Chown(temp.Name(), uid, gid)
	}
	if err == nil {
		err = os.Rename(temp.Name(), path)
	}
	return err
}

// mkdirOwned creates dir and its missing parents, giving the ones it creates
// to uid and gid unless uid is -1.
func mkdirOwned(dir string, uid, gid int) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := mkdirOwned(parent, uid, gid); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if uid != -1 {
		return os.Chown(dir, uid, gid)
	}
	return nil
}

// chooseDataRoot returns the local directory beneath which /get and deldata
// may read and delete data, and a line for the log saying why. Without
// -data-root it is rTorrent's download directory, when rTorrent runs on this
// machine and that directory exists here: rTorrent reports paths on its own
// machine, which mean nothing, or something else, on another.
func chooseDataRoot(ctx context.Context, cfg config, rtorrent *rtapi.Rtorrent) (string, string) {
	switch cfg.dataRoot {
	case dataRootOff:
		return "", "/get and deldata are off (-data-root off)"
	case "":
	default:
		return cfg.dataRoot, "/get and deldata work beneath " + cfg.dataRoot
	}
	if !localAddress(cfg.rtorrentAddress) {
		return "", "/get and deldata are off: rTorrent runs on another machine, so set -data-root to where its data is reachable here"
	}
	stats, err := rtorrent.StatsContext(ctx)
	if err != nil {
		return "", "/get and deldata are off: rTorrent's download directory is unknown (" + err.Error() + "); set -data-root"
	}
	if dir := localDirectory(stats.Directory); dir != "" {
		return dir, "/get and deldata work beneath rTorrent's download directory, " + dir + " (set -data-root to change it, or -data-root off)"
	}
	return "", fmt.Sprintf("/get and deldata are off: rTorrent's download directory %q is not a directory here; set -data-root", stats.Directory)
}

// localAddress reports whether address reaches rTorrent on this machine: a
// socket path, or a loopback host.
func localAddress(address string) bool {
	host := ""
	if parsed, err := url.Parse(address); err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) {
		host = parsed.Hostname()
	} else if splitHost, _, err := net.SplitHostPort(address); err == nil {
		host = splitHost
	} else {
		return true // an SCGI socket path
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localDirectory returns dir, cleaned, if it is an absolute path to a
// directory on this machine, and "" otherwise.
func localDirectory(dir string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return filepath.Clean(dir)
}
