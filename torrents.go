package main

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

// hashPrefixes maps each lowercase hash to its shortest unique prefix of at
// least seven characters. Once sorted, a hash shares its longest common prefix
// with a neighbour, so comparing neighbours keeps this O(n log n).
func hashPrefixes(torrents rtapi.Torrents) map[string]string {
	hashes := make([]string, len(torrents))
	for i, torrent := range torrents {
		hashes[i] = torrent.Hash
	}
	return prefixesOf(hashes)
}

// prefixesOf maps each of hashes, in lower case, to its shortest unique prefix
// of at least seven characters.
func prefixesOf(all []string) map[string]string {
	hashes := make([]string, 0, len(all))
	for _, hash := range all {
		if hash = strings.ToLower(strings.TrimSpace(hash)); hash != "" {
			hashes = append(hashes, hash)
		}
	}
	slices.Sort(hashes)
	result := make(map[string]string, len(hashes))
	for i, hash := range hashes {
		length := 7
		if i > 0 {
			length = max(length, commonPrefixLength(hash, hashes[i-1])+1)
		}
		if i+1 < len(hashes) {
			length = max(length, commonPrefixLength(hash, hashes[i+1])+1)
		}
		result[hash] = hash[:min(length, len(hash))]
	}
	return result
}

func commonPrefixLength(left, right string) int {
	n := 0
	for n < len(left) && n < len(right) && left[n] == right[n] {
		n++
	}
	return n
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

// resolveTorrent returns the one torrent whose hash starts with reference,
// which may be as short as one character. A prefix copied from a list with
// its angle brackets also works.
func resolveTorrent(torrents rtapi.Torrents, reference string) (*rtapi.Torrent, error) {
	reference = strings.ToLower(strings.TrimSpace(reference))
	reference = strings.TrimSuffix(strings.TrimPrefix(reference, "<"), ">")
	if reference == "" {
		return nil, errors.New("a torrent hash prefix is required")
	}
	var match *rtapi.Torrent
	matches := 0
	for _, torrent := range torrents {
		if strings.HasPrefix(strings.ToLower(torrent.Hash), reference) {
			match = torrent
			matches++
		}
	}
	switch matches {
	case 0:
		return nil, fmt.Errorf("no torrent matches hash prefix %q", reference)
	case 1:
		return match, nil
	}
	return nil, fmt.Errorf("hash prefix %q matches %d torrents; give more of the hash", reference, matches)
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
		formatBytes(torrent.UpTotal), timeFromUnix(addedAt(torrent)), formatETA(torrent), trackerHost(torrent.Tracker))
}

// addedAt is when a torrent was added, in Unix seconds, as near as rTorrent
// says: when it first started, which rTorrent remembers. A torrent that has
// never started has only the time rTorrent loaded it, which a restart of
// rTorrent resets.
func addedAt(torrent *rtapi.Torrent) uint64 {
	return cmp.Or(torrent.Started, torrent.Age)
}

// newestFirst orders torrents by when they were added, the newest first.
func newestFirst(x, y *rtapi.Torrent) int {
	return cmp.Compare(addedAt(y), addedAt(x))
}

func timeFromUnix(seconds uint64) string {
	return time.Unix(int64(seconds), 0).Format("2006-01-02 15:04")
}

func formatETA(torrent *rtapi.Torrent) string {
	switch {
	case torrent.Size > 0 && torrent.Completed >= torrent.Size:
		return "done"
	case torrent.ETA == 0:
		return "unknown" // stalled, or the size is not known yet
	}
	return (time.Duration(torrent.ETA) * time.Second).String()
}

func deletionRelative(root, target string) (string, error) {
	if root == "" {
		return "", errors.New("deldata is off; set -data-root to the directory on this machine where rTorrent keeps data")
	}
	return containedRelative(root, target)
}

// containedRelative returns target relative to root, when target is strictly
// inside root.
func containedRelative(root, target string) (string, error) {
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

// dataPath returns where a torrent keeps its data, or "" when rTorrent has not
// said. d.base_path is only reported once rTorrent opens a torrent, so unopened
// torrents are located from d.directory. A single-file torrent's file is named
// after the torrent unless libtorrent had to replace a '/' in the name.
func dataPath(torrent *rtapi.Torrent) string {
	switch {
	case torrent.Path != "":
		return torrent.Path
	case torrent.Directory == "":
		return ""
	case torrent.MultiFile:
		return torrent.Directory
	case torrent.Name == "" || torrent.Name == "." || torrent.Name == ".." || strings.ContainsAny(torrent.Name, `/\`):
		return ""
	default:
		return filepath.Join(torrent.Directory, torrent.Name)
	}
}

// sharedDataConflict refuses deletion when another loaded torrent overlaps the
// target's data, or when rTorrent has not said where another torrent's data is.
func sharedDataConflict(target *rtapi.Torrent, targetPath string, torrents rtapi.Torrents) error {
	for _, other := range torrents {
		if other == target {
			continue
		}
		otherPath := dataPath(other)
		if !filepath.IsAbs(otherPath) {
			return fmt.Errorf("rTorrent did not report where %s keeps its data, so it may share this data; metadata was not deleted", other.Name)
		}
		if pathsOverlap(targetPath, otherPath) {
			return fmt.Errorf("torrent data overlaps %s; metadata was not deleted", other.Name)
		}
	}
	return nil
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
		if err != nil || n <= 0 {
			return 0, errors.New("argument must be a positive number")
		}
	}
	return min(n, total), nil
}
