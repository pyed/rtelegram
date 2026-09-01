package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pyed/rtapi"
)

func formatBytes(bytes uint64) string {
	const unit = uint64(1024)
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exponent := unit, 0
	for quotient := bytes / unit; quotient >= unit && exponent < 5; quotient /= unit {
		div *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exponent])
}

func hashPrefixes(torrents rtapi.Torrents) map[string]string {
	result := make(map[string]string, len(torrents))
	for _, torrent := range torrents {
		hash := strings.ToLower(strings.TrimSpace(torrent.Hash))
		if hash == "" {
			continue
		}
		length := min(7, len(hash))
		for length < len(hash) {
			unique := true
			for _, other := range torrents {
				otherHash := strings.ToLower(strings.TrimSpace(other.Hash))
				if other != torrent && strings.HasPrefix(otherHash, hash[:length]) {
					unique = false
					break
				}
			}
			if unique {
				break
			}
			length++
		}
		result[hash] = hash[:length]
	}
	return result
}

func torrentRef(torrent *rtapi.Torrent, prefixes map[string]string) string {
	if torrent == nil {
		return "unknown"
	}
	hash := strings.ToLower(strings.TrimSpace(torrent.Hash))
	if prefix := prefixes[hash]; prefix != "" {
		return prefix
	}
	return hash
}

func resolveTorrent(torrents rtapi.Torrents, reference string) (*rtapi.Torrent, error) {
	reference = strings.ToLower(strings.TrimSpace(reference))
	if len(reference) < 7 {
		return nil, errors.New("torrent hash prefix must contain at least 7 characters")
	}
	var match *rtapi.Torrent
	for _, torrent := range torrents {
		if strings.HasPrefix(strings.ToLower(torrent.Hash), reference) {
			if match != nil {
				return nil, fmt.Errorf("torrent hash prefix %q is ambiguous", reference)
			}
			match = torrent
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no torrent matches hash prefix %q", reference)
	}
	return match, nil
}

func selectTorrents(torrents rtapi.Torrents, references []string, allowAll bool) (rtapi.Torrents, error) {
	if len(references) == 0 {
		return nil, errors.New("at least one torrent hash is required")
	}
	if allowAll && len(references) == 1 && strings.EqualFold(references[0], "all") {
		if len(torrents) == 0 {
			return nil, errors.New("no torrents are loaded")
		}
		return torrents, nil
	}
	selected := make(rtapi.Torrents, 0, len(references))
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		torrent, err := resolveTorrent(torrents, reference)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(torrent.Hash)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		selected = append(selected, torrent)
	}
	return selected, nil
}

func trackerHost(tracker *url.URL) string {
	if tracker == nil || tracker.Hostname() == "" {
		return "unknown"
	}
	return tracker.Hostname()
}

func formatTorrent(torrent *rtapi.Torrent, reference string) string {
	return fmt.Sprintf("<%s> %s\n%s %s (%s) ↓ %s ↑ %s R: %.2f",
		reference, torrent.Name, torrent.State, formatBytes(torrent.Completed), torrent.Percent,
		formatBytes(torrent.DownRate), formatBytes(torrent.UpRate), torrent.Ratio)
}

func formatTorrentInfo(torrent *rtapi.Torrent) string {
	return fmt.Sprintf("%s\n%s %s (%s) ↓ %s ↑ %s R: %.2f UP: %s\nAdded: %s, ETA: %s\nTracker: %s",
		torrent.Name, torrent.State, formatBytes(torrent.Completed), torrent.Percent,
		formatBytes(torrent.DownRate), formatBytes(torrent.UpRate), torrent.Ratio,
		formatBytes(torrent.UpTotal), timeFromUnix(torrent.Age), formatETA(torrent.ETA), trackerHost(torrent.Tracker))
}

func timeFromUnix(seconds uint64) string {
	return time.Unix(int64(seconds), 0).Format(time.Stamp)
}

func formatETA(seconds uint64) string {
	if seconds == 0 {
		return "unknown"
	}
	return (time.Duration(seconds) * time.Second).String()
}

func deletionRelative(root, target string) (string, error) {
	if root == "" {
		return "", errors.New("deldata is disabled; configure an absolute -data-root")
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) {
		return "", errors.New("data root and torrent path must be absolute")
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return "", fmt.Errorf("compare data path with configured root: %w", err)
	}
	if relative == "." || relative == "" || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("torrent data path is not strictly inside the configured data root")
	}
	return relative, nil
}

func pathsOverlap(left, right string) bool {
	if left == "" || right == "" || !filepath.IsAbs(left) || !filepath.IsAbs(right) {
		return false
	}
	left = canonicalPath(left)
	right = canonicalPath(right)
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
			return true
		}
	}
	return false
}

func canonicalPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func validateTorrentData(rootPath, targetPath string) (*os.Root, string, error) {
	relative, err := deletionRelative(rootPath, targetPath)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", fmt.Errorf("open data root: %w", err)
	}
	info, err := root.Lstat(relative)
	if err != nil {
		root.Close()
		return nil, "", fmt.Errorf("validate torrent data path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		root.Close()
		return nil, "", errors.New("torrent data path must not be a symbolic link")
	}
	return root, relative, nil
}

func pluralNames(torrents rtapi.Torrents) string {
	names := make([]string, len(torrents))
	for i, torrent := range torrents {
		names[i] = torrent.Name
	}
	return strings.Join(names, ", ")
}

func parseCount(tokens []string, fallback, total int) (int, error) {
	n := fallback
	if len(tokens) > 0 {
		var err error
		n, err = strconv.Atoi(tokens[0])
		if err != nil {
			return 0, errors.New("argument must be a number")
		}
	}
	if n <= 0 || n > total {
		n = total
	}
	return n, nil
}
