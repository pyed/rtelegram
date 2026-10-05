package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
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
	// maxSingleEvents is how many events of a kind one check announces in
	// messages of their own; more come in one message, which lists up to
	// maxListedEvents of them.
	maxSingleEvents = 3
	maxListedEvents = 10
	// maxErrorKinds is how many groups of errors a message lists.
	maxErrorKinds = 10
	// errorHold is how long new errors wait after others with the same
	// tracker and message were announced, so that a tracker failing for
	// every torrent is announced once rather than for each. It doubles while
	// such errors keep coming, up to maxErrorHold.
	errorHold    = time.Hour
	maxErrorHold = 24 * time.Hour
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
	text string // the message announcing the event on its own
	line string // the event's line in a message announcing several
	hash string // the torrent, for a details button; empty for disk events
}

// watcher remembers the previous check, so changes are reported once.
type watcher struct {
	// idle is set while no chat is subscribed and checks are skipped.
	idle      bool
	started   bool
	errored   map[string]bool
	progress  map[string]progressMark
	lowDisk   bool
	lastError string
	// notices hold back new errors like ones announced lately.
	notices map[errorKey]*errorNotice
}

// errorKey is what errors are grouped by: the tracker, and its message.
type errorKey struct{ tracker, message string }

func errorKeyOf(torrent *rtapi.Torrent) errorKey {
	return errorKey{trackerHost(torrent.Tracker), strings.TrimSpace(torrent.Message)}
}

func compareErrorKeys(x, y errorKey) int {
	return cmp.Or(cmp.Compare(x.tracker, y.tracker), cmp.Compare(x.message, y.message))
}

// An errorNotice holds back new errors with the tracker and message of ones
// just announced.
type errorNotice struct {
	until time.Time                 // new errors wait until then
	hold  time.Duration             // the wait after the next announcement
	held  map[string]*rtapi.Torrent // the errors waiting, by hash
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
	// With nobody to tell, skip the check, which lists every torrent. Once a
	// chat subscribes, look afresh, so it is not told what happened meanwhile.
	if !a.subscribed() {
		*w = watcher{idle: true}
		return
	}
	if w.idle {
		w.idle = false
		if err := a.state.update(func(data *stateData) { data.CompletedWatermark, data.CompletedAt = 0, nil }); err != nil {
			a.logger.Printf("[ERROR] saving completed torrents: %s", err)
		}
	}
	torrents, err := a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
	if err != nil {
		if ctx.Err() == nil && err.Error() != w.lastError {
			a.logger.Printf("[ERROR] watching rTorrent: %s", err)
		}
		w.lastError = err.Error()
		return
	}
	w.lastError = ""
	errored, stalled := w.torrentEvents(torrents, now, a.stallAfter)
	events := together(a.completedEvents(torrents), "✅ Completed: %d downloads")
	events = append(events, a.errorEvents(ctx, w, errored, torrents, now)...)
	events = append(events, together(stalled, "🐢 Stalled: %d downloads")...)
	events = append(events, a.diskEvents(ctx, w, torrents)...)
	w.started = true
	a.deliver(ctx, events)
}

// together returns events of one kind as they are, or, when there are more
// than maxSingleEvents, as one event that lists them under heading, which
// says how many there are.
func together(events []event, heading string) []event {
	if len(events) <= maxSingleEvents {
		return events
	}
	var text strings.Builder
	fmt.Fprintf(&text, heading, len(events))
	for i, e := range events {
		if i == maxListedEvents {
			fmt.Fprintf(&text, "\n• and %d more", len(events)-i)
			break
		}
		text.WriteString("\n• " + e.line)
	}
	return []event{{kind: events[0].kind, text: text.String()}}
}

// wants reports whether any chat wants events of kind.
func (a *application) wants(kind string) bool {
	wanted := false
	a.state.read(func(data *stateData) {
		for _, settings := range data.Notify {
			wanted = wanted || slices.Contains(settings.Events, kind)
		}
	})
	return wanted
}

// subscribed reports whether any chat wants notifications.
func (a *application) subscribed() bool {
	subscribed := false
	a.state.read(func(data *stateData) {
		for _, settings := range data.Notify {
			subscribed = subscribed || len(settings.Events) > 0
		}
	})
	return subscribed
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
			events = append(events, event{kind: eventCompleted,
				text: fmt.Sprintf("✅ Completed: %s\n%s, ratio %.2f", torrent.Name, formatBytes(torrent.Size), torrent.Ratio),
				line: fmt.Sprintf("%s, %s", torrent.Name, formatBytes(torrent.Size)), hash: torrent.Hash})
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

// torrentEvents returns the torrents that newly have an error, and events for
// downloads that have made no progress for stallAfter.
func (w *watcher) torrentEvents(torrents rtapi.Torrents, now time.Time, stallAfter time.Duration) (rtapi.Torrents, []event) {
	var fresh rtapi.Torrents
	var events []event
	errored := make(map[string]bool)
	progress := make(map[string]progressMark)
	for _, torrent := range torrents {
		if torrent.State == rtapi.Error {
			errored[torrent.Hash] = true
			if w.started && !w.errored[torrent.Hash] {
				fresh = append(fresh, torrent)
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
			stalled := now.Sub(mark.since).Round(time.Minute)
			events = append(events, event{kind: eventStalled,
				text: fmt.Sprintf("🐢 Stalled: %s\nNo progress for %s, at %s", torrent.Name, stalled, torrent.Percent),
				line: fmt.Sprintf("%s, at %s, no progress for %s", torrent.Name, torrent.Percent, stalled), hash: torrent.Hash})
		}
		progress[torrent.Hash] = mark
	}
	w.errored, w.progress = errored, progress
	return fresh, events
}

// errorEvents announces fresh, the torrents that newly have errors. An error
// whose tracker and message were announced lately waits until the hold ends,
// and then comes with the others that came meanwhile, unless it has cleared.
// A tracker that fails for every torrent is so announced once rather than
// for each torrent, however slowly they report the failure.
func (a *application) errorEvents(ctx context.Context, w *watcher, fresh, torrents rtapi.Torrents, now time.Time) []event {
	if !a.wants(eventErrors) {
		w.notices = nil
		return nil
	}
	if len(fresh) > 0 {
		if err := a.rtorrent.TrackersContext(ctx, fresh); err != nil {
			a.logger.Printf("[ERROR] trackers of torrents with new errors: %s", err)
		}
	}
	if w.notices == nil {
		w.notices = make(map[errorKey]*errorNotice)
	}
	var due rtapi.Torrents
	announced := make(map[string]bool)
	announce := func(torrent *rtapi.Torrent) {
		if !announced[torrent.Hash] {
			announced[torrent.Hash] = true
			due = append(due, torrent)
		}
	}
	for _, torrent := range fresh {
		// It has cleared since any hold it was waiting in.
		for _, notice := range w.notices {
			delete(notice.held, torrent.Hash)
		}
		if notice := w.notices[errorKeyOf(torrent)]; notice != nil && now.Before(notice.until) {
			notice.held[torrent.Hash] = torrent
			continue
		}
		announce(torrent)
	}
	// When a hold ends, the errors it held come, unless they have cleared.
	current := make(map[string]*rtapi.Torrent, len(torrents))
	for _, torrent := range torrents {
		current[torrent.Hash] = torrent
	}
	for _, key := range slices.SortedFunc(maps.Keys(w.notices), compareErrorKeys) {
		notice := w.notices[key]
		if now.Before(notice.until) {
			continue
		}
		waiting := false
		for _, hash := range slices.Sorted(maps.Keys(notice.held)) {
			if torrent := current[hash]; torrent != nil && torrent.State == rtapi.Error {
				torrent.Tracker = notice.held[hash].Tracker
				announce(torrent)
				waiting = true
			}
		}
		notice.held = make(map[string]*rtapi.Torrent)
		if !waiting {
			delete(w.notices, key) // calm again: the next such error comes at once
		}
	}
	// What is announced now holds back more of the same, longer each time.
	for _, torrent := range due {
		key := errorKeyOf(torrent)
		notice := w.notices[key]
		if notice == nil {
			notice = &errorNotice{hold: errorHold}
			w.notices[key] = notice
		}
		if notice.until.After(now) {
			continue // already renewed for another torrent
		}
		notice.until, notice.hold = now.Add(notice.hold), min(2*notice.hold, maxErrorHold)
		notice.held = make(map[string]*rtapi.Torrent)
	}

	if len(due) > maxSingleEvents {
		return []event{{kind: eventErrors, text: fmt.Sprintf("⚠️ Errors: %d torrents", len(due)) + errorGroups(due) +
			"\n\n/errors lists every torrent with an error."}}
	}
	events := make([]event, len(due))
	for i, torrent := range due {
		events[i] = event{kind: eventErrors, text: fmt.Sprintf("⚠️ Error: %s\n%s", torrent.Name, torrent.Message), hash: torrent.Hash}
	}
	return events
}

// errorGroups lists errored torrents by tracker and message, a line each
// starting "\n• ": torrents that share both are counted together, largest
// group first, up to maxErrorKinds groups.
func errorGroups(errored rtapi.Torrents) string {
	groups := make(map[errorKey]rtapi.Torrents)
	for _, torrent := range errored {
		key := errorKeyOf(torrent)
		groups[key] = append(groups[key], torrent)
	}
	kinds := slices.SortedFunc(maps.Keys(groups), func(x, y errorKey) int {
		return cmp.Or(cmp.Compare(len(groups[y]), len(groups[x])), compareErrorKeys(x, y))
	})
	var text strings.Builder
	for _, key := range kinds[:min(len(kinds), maxErrorKinds)] {
		if torrents := groups[key]; len(torrents) == 1 {
			fmt.Fprintf(&text, "\n• %s: %s (%s)", key.tracker, key.message, torrents[0].Name)
		} else {
			fmt.Fprintf(&text, "\n• %s, %d torrents: %s", key.tracker, len(torrents), key.message)
		}
	}
	if more := len(kinds) - maxErrorKinds; more > 0 {
		fmt.Fprintf(&text, "\n• and %d other kinds of error", more)
	}
	return text.String()
}

// diskEvents warns once when free space where downloads land drops below
// lowDisk, and again only after it recovers by a tenth. It looks only while
// a chat wants to know.
func (a *application) diskEvents(ctx context.Context, w *watcher, torrents rtapi.Torrents) []event {
	if a.lowDisk == 0 || !a.wants(eventDisk) {
		return nil
	}
	free, err := a.freeSpace(ctx, torrents)
	switch {
	case err != nil:
	case free < a.lowDisk && !w.lowDisk:
		w.lowDisk = true
		return []event{{kind: eventDisk, text: fmt.Sprintf("💾 Low disk space: %s free where rTorrent saves data.", formatBytes(free))}}
	case free >= a.lowDisk+a.lowDisk/10:
		w.lowDisk = false
	}
	return nil
}

// freeSpace returns the free disk space where rTorrent saves data: the least
// it reports where up to maxDiskChecks of spaceTorrents keep theirs, asked in
// one request.
func (a *application) freeSpace(ctx context.Context, torrents rtapi.Torrents) (uint64, error) {
	check := spaceTorrents(torrents)
	if len(check) == 0 {
		return 0, errors.New("no active torrent to ask through")
	}
	hashes := make([]string, 0, maxDiskChecks)
	for _, torrent := range check[:min(len(check), maxDiskChecks)] {
		hashes = append(hashes, torrent.Hash)
	}
	spaces, err := a.rtorrent.FreeDiskSpacesContext(ctx, hashes...)
	if err != nil {
		return 0, err
	}
	return slices.Min(spaces), nil
}

// spaceTorrents returns the torrents through which rTorrent can report free
// disk space. It learns where a torrent's data is only when it opens the
// torrent, and reports 0 until then; active torrents are always open.
// Downloads come first, as they are what fills the disk.
func spaceTorrents(torrents rtapi.Torrents) rtapi.Torrents {
	if downloading := filterTorrents(torrents, func(torrent *rtapi.Torrent) bool { return torrent.State == rtapi.Leeching }); len(downloading) > 0 {
		return downloading
	}
	return filterTorrents(torrents, func(torrent *rtapi.Torrent) bool {
		return torrent.State == rtapi.Seeding || torrent.State == rtapi.Error
	})
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
			if err != nil && chatGone(err) {
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
