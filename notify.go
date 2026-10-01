package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

const (
	eventCompleted = "completed"
	eventErrors    = "errors"
	eventStalled   = "stalled"
	eventDisk      = "disk"

	defaultWatchInterval = 30 * time.Second
	defaultStallAfter    = 30 * time.Minute
	// Free space is checked where at most this many downloads land.
	maxDiskChecks = 10
)

// notifyEvents are the events a chat can subscribe to, as /notify lists them.
var notifyEvents = []struct{ name, label string }{
	{eventCompleted, "Completed downloads"},
	{eventErrors, "New errors"},
	{eventStalled, "Stalled downloads"},
	{eventDisk, "Low disk space"},
}

// An event is something the watcher noticed.
type event struct {
	kind string
	text string
	hash string // the torrent, for a details button; empty for disk events
}

// watcher remembers the previous check, so changes are reported once.
type watcher struct {
	started   bool
	errored   map[string]bool
	progress  map[string]progressMark
	lowDisk   bool
	lastError string
}

type progressMark struct {
	completed uint64
	since     time.Time
	announced bool
}

// watchEvents checks rTorrent for events every watch interval until ctx ends.
func (a *application) watchEvents(ctx context.Context) {
	w := &watcher{}
	for {
		a.checkEvents(ctx, w, time.Now())
		if !waitFor(ctx, a.watchInterval) {
			return
		}
	}
}

// checkEvents compares rTorrent with the previous check and notifies
// subscribed chats of what changed.
func (a *application) checkEvents(ctx context.Context, w *watcher, now time.Time) {
	torrents, err := a.rtorrent.TorrentsContext(ctx)
	if err != nil {
		if ctx.Err() == nil && err.Error() != w.lastError {
			a.logger.Printf("[ERROR] watching rTorrent: %s", err)
		}
		w.lastError = err.Error()
		return
	}
	w.lastError = ""
	events := a.completedEvents(torrents)
	events = append(events, w.torrentEvents(torrents, now, a.stallAfter)...)
	events = append(events, a.diskEvents(ctx, w, torrents)...)
	w.started = true
	a.deliver(ctx, events)
}

// completedEvents announces torrents that finished since the watermark kept
// in the state, so completions while the bot was offline are announced once.
func (a *application) completedEvents(torrents rtapi.Torrents) []event {
	var watermark uint64
	var announced []string
	a.state.read(func(data *stateData) {
		watermark, announced = data.CompletedWatermark, slices.Clone(data.CompletedAt)
	})
	finished := filterTorrents(torrents, func(torrent *rtapi.Torrent) bool { return torrent.Finished > 0 })
	slices.SortStableFunc(finished, func(x, y *rtapi.Torrent) int { return cmp.Compare(x.Finished, y.Finished) })

	var events []event
	newest := max(watermark, 1) // non-zero once the watcher has looked
	for _, torrent := range finished {
		fresh := torrent.Finished > watermark || (torrent.Finished == watermark && !slices.Contains(announced, torrent.Hash))
		if watermark != 0 && fresh {
			events = append(events, event{eventCompleted,
				fmt.Sprintf("✅ Completed: %s\n%s, ratio %.2f", torrent.Name, formatBytes(torrent.Size), torrent.Ratio), torrent.Hash})
		}
		newest = max(newest, torrent.Finished)
	}
	var atNewest []string
	for _, torrent := range finished {
		if torrent.Finished == newest {
			atNewest = append(atNewest, torrent.Hash)
		}
	}
	if newest != watermark || !slices.Equal(atNewest, announced) {
		if err := a.state.update(func(data *stateData) {
			data.CompletedWatermark, data.CompletedAt = newest, atNewest
		}); err != nil {
			a.logger.Printf("[ERROR] saving completed torrents: %s", err)
		}
	}
	return events
}

// torrentEvents finds torrents that newly have an error, and downloads that
// have made no progress for stallAfter.
func (w *watcher) torrentEvents(torrents rtapi.Torrents, now time.Time, stallAfter time.Duration) []event {
	var events []event
	errored := make(map[string]bool)
	progress := make(map[string]progressMark)
	for _, torrent := range torrents {
		if torrent.State == rtapi.Error {
			errored[torrent.Hash] = true
			if w.started && !w.errored[torrent.Hash] {
				events = append(events, event{eventErrors, fmt.Sprintf("⚠️ Error: %s\n%s", torrent.Name, torrent.Message), torrent.Hash})
			}
		}
		if torrent.State != rtapi.Leeching || stallAfter <= 0 {
			continue
		}
		mark, seen := w.progress[torrent.Hash]
		switch {
		case !seen || mark.completed != torrent.Completed:
			mark = progressMark{completed: torrent.Completed, since: now}
		case !mark.announced && now.Sub(mark.since) >= stallAfter:
			mark.announced = true
			events = append(events, event{eventStalled, fmt.Sprintf("🐢 Stalled: %s\nNo progress for %s, at %s",
				torrent.Name, now.Sub(mark.since).Round(time.Minute), torrent.Percent), torrent.Hash})
		}
		progress[torrent.Hash] = mark
	}
	w.errored, w.progress = errored, progress
	return events
}

// diskEvents warns once when free space where downloads land drops below
// lowDisk, and again only after it recovers by a tenth.
func (a *application) diskEvents(ctx context.Context, w *watcher, torrents rtapi.Torrents) []event {
	if a.lowDisk == 0 || len(torrents) == 0 {
		return nil
	}
	check := filterTorrents(torrents, func(torrent *rtapi.Torrent) bool { return torrent.State == rtapi.Leeching })
	if len(check) == 0 {
		check = rtapi.Torrents{slices.MaxFunc(torrents, func(x, y *rtapi.Torrent) int { return cmp.Compare(x.Age, y.Age) })}
	}
	free := uint64(math.MaxUint64)
	for _, torrent := range check[:min(len(check), maxDiskChecks)] {
		if space, err := a.rtorrent.FreeDiskSpaceContext(ctx, torrent.Hash); err == nil {
			free = min(free, space)
		}
	}
	switch {
	case free == math.MaxUint64:
	case free < a.lowDisk && !w.lowDisk:
		w.lowDisk = true
		return []event{{eventDisk, fmt.Sprintf("💾 Low disk space: %s free where rTorrent saves data.", formatBytes(free)), ""}}
	case free >= a.lowDisk+a.lowDisk/10:
		w.lowDisk = false
	}
	return nil
}

// deliver sends each event to the chats subscribed to it. A chat that has
// blocked or removed the bot is unsubscribed.
func (a *application) deliver(ctx context.Context, events []event) {
	if len(events) == 0 {
		return
	}
	var subscribers map[int64]notifySettings
	a.state.read(func(data *stateData) { subscribers = maps.Clone(data.Notify) })
	for _, chatID := range slices.Sorted(maps.Keys(subscribers)) {
		settings := subscribers[chatID]
		chatCtx := ctx
		if settings.Thread != 0 {
			chatCtx = context.WithValue(ctx, messageThreadIDKey{}, settings.Thread)
		}
		for _, e := range events {
			if !slices.Contains(settings.Events, e.kind) {
				continue
			}
			var err error
			if e.hash == "" {
				_, err = a.send(chatCtx, chatID, e.text)
			} else {
				details := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{button("ℹ Details", "t:"+e.hash)}}}
				err = a.sendScreen(chatCtx, chatID, e.text, details, &screen{})
			}
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "forbidden") {
				a.logger.Printf("[INFO] stopped notifying chat %d: %s", chatID, err)
				a.state.update(func(data *stateData) { delete(data.Notify, chatID) })
				break
			}
		}
	}
}

// notify shows or changes which events this chat is told about.
func (a *application) notify(ctx context.Context, chatID int64, arguments []string) {
	switch strings.ToLower(strings.Join(arguments, " ")) {
	case "":
	case "on", "all":
		a.setNotify(ctx, chatID, func(map[string]bool) map[string]bool { return allEvents() })
	case "off", "none":
		a.setNotify(ctx, chatID, func(map[string]bool) map[string]bool { return nil })
	default:
		a.send(ctx, chatID, "notify: use notify, notify on, or notify off")
		return
	}
	text, keyboard := a.renderNotify(chatID)
	a.sendScreen(ctx, chatID, text, keyboard, &screen{notify: true})
}

func allEvents() map[string]bool {
	all := make(map[string]bool)
	for _, e := range notifyEvents {
		all[e.name] = true
	}
	return all
}

// setNotify changes this chat's subscriptions and saves them, posting in the
// forum topic the change was made from.
func (a *application) setNotify(ctx context.Context, chatID int64, change func(map[string]bool) map[string]bool) error {
	thread, _ := ctx.Value(messageThreadIDKey{}).(int)
	return a.state.update(func(data *stateData) {
		current := make(map[string]bool)
		for _, name := range data.Notify[chatID].Events {
			current[name] = true
		}
		next := change(current)
		var events []string
		for _, e := range notifyEvents {
			if next[e.name] {
				events = append(events, e.name)
			}
		}
		if len(events) == 0 {
			delete(data.Notify, chatID)
			return
		}
		if data.Notify == nil {
			data.Notify = make(map[int64]notifySettings)
		}
		data.Notify[chatID] = notifySettings{Events: events, Thread: thread}
	})
}

func (a *application) renderNotify(chatID int64) (string, *models.InlineKeyboardMarkup) {
	var subscribed []string
	a.state.read(func(data *stateData) { subscribed = slices.Clone(data.Notify[chatID].Events) })
	var rows [][]models.InlineKeyboardButton
	for _, e := range notifyEvents {
		mark := "⬜ "
		if slices.Contains(subscribed, e.name) {
			mark = "✅ "
		}
		rows = append(rows, []models.InlineKeyboardButton{button(mark+e.label, "nt:"+e.name)})
	}
	rows = append(rows, []models.InlineKeyboardButton{button("All on", "nt:on"), button("All off", "nt:off")})
	return "Notifications for this chat. Tap one to turn it on or off.", &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// pressNotify toggles a subscription from the /notify screen.
func (a *application) pressNotify(ctx context.Context, key screenKey, choice string) (string, bool) {
	err := a.setNotify(ctx, key.chatID, func(current map[string]bool) map[string]bool {
		switch choice {
		case "on":
			return allEvents()
		case "off":
			return nil
		}
		current[choice] = !current[choice]
		return current
	})
	text, keyboard := a.renderNotify(key.chatID)
	if editErr := a.editScreen(ctx, key, text, keyboard); editErr != nil {
		return editErr.Error(), true
	}
	if err != nil {
		return "Changed, but not saved, so a restart will forget it: " + err.Error(), true
	}
	return "", false
}
