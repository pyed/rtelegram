package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pyed/rtapi"
)

func renderTorrentListWithPrefixes(torrents rtapi.Torrents, prefixes map[string]string) string {
	var output strings.Builder
	for _, torrent := range torrents {
		fmt.Fprintf(&output, "<%s> %s\n", torrentRef(torrent, prefixes), torrent.Name)
	}
	return output.String()
}

func renderTorrentDetailsWithPrefixes(torrents rtapi.Torrents, prefixes map[string]string) string {
	var output strings.Builder
	for _, torrent := range torrents {
		output.WriteString(formatTorrent(torrent, torrentRef(torrent, prefixes)))
		output.WriteString("\n\n")
	}
	return output.String()
}

func filterTorrents(torrents rtapi.Torrents, keep func(*rtapi.Torrent) bool) rtapi.Torrents {
	filtered := make(rtapi.Torrents, 0, len(torrents))
	for _, torrent := range torrents {
		if keep(torrent) {
			filtered = append(filtered, torrent)
		}
	}
	return filtered
}

func (a *application) list(ctx context.Context, chatID int64, arguments []string) {
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, "list: "+err.Error())
		return
	}
	prefixes := hashPrefixes(torrents)
	if len(arguments) > 0 {
		query := strings.ToLower(strings.Join(arguments, " "))
		torrents = filterTorrents(torrents, func(torrent *rtapi.Torrent) bool {
			return strings.Contains(strings.ToLower(trackerHost(torrent.Tracker)), query)
		})
	}
	if len(torrents) == 0 {
		a.send(ctx, chatID, "list: No torrents")
		return
	}
	a.send(ctx, chatID, renderTorrentListWithPrefixes(torrents, prefixes))
}

func (a *application) head(ctx context.Context, chatID int64, arguments []string) {
	n := 5
	if len(arguments) > 0 {
		var err error
		n, err = parseCount(arguments, 5, int(^uint(0)>>1))
		if err != nil {
			a.send(ctx, chatID, "head: "+err.Error())
			return
		}
	}
	a.liveList(ctx, chatID, "head", func(torrents rtapi.Torrents) rtapi.Torrents {
		limit := min(max(n, 0), len(torrents))
		return torrents[:limit]
	})
}

func (a *application) tail(ctx context.Context, chatID int64, arguments []string) {
	n := 5
	if len(arguments) > 0 {
		var err error
		n, err = parseCount(arguments, 5, int(^uint(0)>>1))
		if err != nil {
			a.send(ctx, chatID, "tail: "+err.Error())
			return
		}
	}
	a.liveList(ctx, chatID, "tail", func(torrents rtapi.Torrents) rtapi.Torrents {
		limit := min(max(n, 0), len(torrents))
		return torrents[len(torrents)-limit:]
	})
}

func (a *application) active(ctx context.Context, chatID int64) {
	a.liveList(ctx, chatID, "active", func(torrents rtapi.Torrents) rtapi.Torrents {
		return filterTorrents(torrents, func(torrent *rtapi.Torrent) bool {
			return torrent.DownRate > 0 || torrent.UpRate > 0
		})
	})
}

func (a *application) liveList(ctx context.Context, chatID int64, label string, selectView func(rtapi.Torrents) rtapi.Torrents) {
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, label+": "+err.Error())
		return
	}
	view := selectView(torrents)
	text := renderTorrentDetailsWithPrefixes(view, hashPrefixes(torrents))
	if text == "" {
		text = "No " + label + " torrents"
	}
	messageID, err := a.send(ctx, chatID, text)
	if err != nil || messageID == 0 || a.noLive {
		return
	}
	a.launch(ctx, func(liveCtx context.Context) {
		for range a.duration {
			if !a.wait(liveCtx) {
				return
			}
			updated, err := a.torrents(chatID)
			if err != nil {
				a.logger.Printf("%s: %s", label, err)
				continue
			}
			next := renderTorrentDetailsWithPrefixes(selectView(updated), hashPrefixes(updated))
			if next == "" {
				next = "No " + label + " torrents"
			}
			if err := a.editChanged(liveCtx, chatID, messageID, &text, next); err != nil && liveCtx.Err() == nil {
				return
			}
		}
	})
}

func (a *application) latest(ctx context.Context, chatID int64, arguments []string) {
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, "latest: "+err.Error())
		return
	}
	n, err := parseCount(arguments, 5, len(torrents))
	if err != nil {
		a.send(ctx, chatID, "latest: "+err.Error())
		return
	}
	prefixes := hashPrefixes(torrents)
	torrents = slices.Clone(torrents)
	slices.SortStableFunc(torrents, func(a, b *rtapi.Torrent) int { return cmp.Compare(b.Age, a.Age) })
	if n == 0 {
		a.send(ctx, chatID, "latest: No torrents")
		return
	}
	a.send(ctx, chatID, renderTorrentListWithPrefixes(torrents[:n], prefixes))
}

func (a *application) search(ctx context.Context, chatID int64, arguments []string) {
	if len(arguments) == 0 {
		a.send(ctx, chatID, "search: needs an argument")
		return
	}
	query := strings.ToLower(strings.Join(arguments, " "))
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, "search: "+err.Error())
		return
	}
	prefixes := hashPrefixes(torrents)
	torrents = filterTorrents(torrents, func(torrent *rtapi.Torrent) bool {
		return strings.Contains(strings.ToLower(torrent.Name), query)
	})
	if len(torrents) == 0 {
		a.send(ctx, chatID, "No matches")
		return
	}
	a.send(ctx, chatID, renderTorrentListWithPrefixes(torrents, prefixes))
}

func (a *application) sendStatus(ctx context.Context, chatID int64, label, empty string, keep func(*rtapi.Torrent) bool) {
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, label+": "+err.Error())
		return
	}
	prefixes := hashPrefixes(torrents)
	torrents = filterTorrents(torrents, keep)
	if len(torrents) == 0 {
		a.send(ctx, chatID, empty)
		return
	}
	a.send(ctx, chatID, renderTorrentListWithPrefixes(torrents, prefixes))
}

func (a *application) downs(ctx context.Context, chatID int64) {
	a.sendStatus(ctx, chatID, "down", "No downloads", func(t *rtapi.Torrent) bool { return t.State == rtapi.Leeching })
}

func (a *application) seeding(ctx context.Context, chatID int64) {
	a.sendStatus(ctx, chatID, "seeding", "No torrents seeding", func(t *rtapi.Torrent) bool { return t.State == rtapi.Seeding })
}

func (a *application) hashing(ctx context.Context, chatID int64) {
	a.sendStatus(ctx, chatID, "checking", "No torrents checking", func(t *rtapi.Torrent) bool { return t.State == rtapi.Hashing })
}

func (a *application) errors(ctx context.Context, chatID int64) {
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, "errors: "+err.Error())
		return
	}
	prefixes := hashPrefixes(torrents)
	torrents = filterTorrents(torrents, func(t *rtapi.Torrent) bool { return t.State == rtapi.Error })
	if len(torrents) == 0 {
		a.send(ctx, chatID, "No errors")
		return
	}
	var output strings.Builder
	for _, torrent := range torrents {
		fmt.Fprintf(&output, "<%s> %s\n%s\n\n", torrentRef(torrent, prefixes), torrent.Name, torrent.Message)
	}
	a.send(ctx, chatID, output.String())
}

func (a *application) paused(ctx context.Context, chatID int64) {
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, "paused: "+err.Error())
		return
	}
	prefixes := hashPrefixes(torrents)
	torrents = filterTorrents(torrents, func(t *rtapi.Torrent) bool { return t.State == rtapi.Stopped })
	if len(torrents) == 0 {
		a.send(ctx, chatID, "No paused torrents")
		return
	}
	a.send(ctx, chatID, renderTorrentDetailsWithPrefixes(torrents, prefixes))
}

func (a *application) info(ctx context.Context, chatID int64, references []string) {
	torrents, err := a.selected(chatID, references, false)
	if err != nil {
		a.send(ctx, chatID, "info: "+err.Error())
		return
	}
	for _, torrent := range torrents {
		text := formatTorrentInfo(torrent)
		messageID, err := a.send(ctx, chatID, text)
		if err != nil || a.noLive || messageID == 0 {
			continue
		}
		hash := torrent.Hash
		a.launch(ctx, func(liveCtx context.Context) {
			for range a.duration {
				if !a.wait(liveCtx) {
					return
				}
				updated, err := a.rtorrent.GetTorrent(hash)
				if err != nil {
					a.logger.Printf("info: %s", err)
					return
				}
				next := formatTorrentInfo(updated)
				if err := a.editChanged(liveCtx, chatID, messageID, &text, next); err != nil && liveCtx.Err() == nil {
					return
				}
			}
		})
	}
}

func (a *application) trackers(ctx context.Context, chatID int64) {
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, "trackers: "+err.Error())
		return
	}
	counts := make(map[string]int)
	for _, torrent := range torrents {
		counts[trackerHost(torrent.Tracker)]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var output strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&output, "%d - %s\n", counts[key], key)
	}
	if output.Len() == 0 {
		a.send(ctx, chatID, "No trackers")
		return
	}
	a.send(ctx, chatID, output.String())
}

func (a *application) stats(ctx context.Context, chatID int64) {
	statistics, err := a.rtorrent.Stats()
	if err != nil {
		a.send(ctx, chatID, "stats: "+err.Error())
		return
	}
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, "stats: "+err.Error())
		return
	}
	var loadedUp, loadedDown uint64
	for _, torrent := range torrents {
		loadedUp += torrent.UpTotal
		loadedDown += torrent.Completed
	}
	ratio := 0.0
	if loadedDown != 0 {
		ratio = float64(loadedUp) / float64(loadedDown)
	}
	throttleUp, throttleDown := "off", "off"
	if statistics.ThrottleUp != 0 {
		throttleUp = formatBytes(statistics.ThrottleUp)
	}
	if statistics.ThrottleDown != 0 {
		throttleDown = formatBytes(statistics.ThrottleDown)
	}
	a.send(ctx, chatID, fmt.Sprintf("Throttle: %s / %s\nPort: %s\nDirectory: %s\nSession uploaded: %s\nSession downloaded: %s\nLoaded torrents uploaded: %s\nLoaded torrents downloaded: %s\nLoaded torrents ratio: %.2f",
		throttleUp, throttleDown, statistics.Port, statistics.Directory,
		formatBytes(statistics.TotalUp), formatBytes(statistics.TotalDown),
		formatBytes(loadedUp), formatBytes(loadedDown), ratio))
}

func (a *application) count(ctx context.Context, chatID int64) {
	torrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, "count: "+err.Error())
		return
	}
	counts := map[string]int{}
	for _, torrent := range torrents {
		counts[torrent.State]++
	}
	a.send(ctx, chatID, fmt.Sprintf("Leeching: %d\nSeeding: %d\nComplete: %d\nStopped: %d\nHashing: %d\nError: %d\n\nTotal: %d",
		counts[rtapi.Leeching], counts[rtapi.Seeding], counts[rtapi.Complete], counts[rtapi.Stopped], counts[rtapi.Hashing], counts[rtapi.Error], len(torrents)))
}

type speedReporter interface {
	SpeedsWithError() (down, up uint64, err error)
}

func currentSpeeds(target *rtapi.Rtorrent) (uint64, uint64, error) {
	if reporter, ok := any(target).(speedReporter); ok {
		return reporter.SpeedsWithError()
	}
	down, up := target.Speeds()
	return down, up, nil
}

func (a *application) speed(ctx context.Context, chatID int64) {
	down, up, err := currentSpeeds(a.rtorrent)
	if err != nil {
		a.send(ctx, chatID, "speed: "+err.Error())
		return
	}
	text := fmt.Sprintf("↓ %s ↑ %s", formatBytes(down), formatBytes(up))
	messageID, err := a.send(ctx, chatID, text)
	if err != nil || a.noLive || messageID == 0 {
		return
	}
	a.launch(ctx, func(liveCtx context.Context) {
		for range a.duration {
			if !a.wait(liveCtx) {
				return
			}
			down, up, err := currentSpeeds(a.rtorrent)
			if err != nil {
				a.edit(liveCtx, chatID, messageID, "speed: "+err.Error())
				return
			}
			next := fmt.Sprintf("↓ %s ↑ %s", formatBytes(down), formatBytes(up))
			if err := a.editChanged(liveCtx, chatID, messageID, &text, next); err != nil && liveCtx.Err() == nil {
				return
			}
		}
		a.edit(liveCtx, chatID, messageID, "↓ - ↑ -")
	})
}
