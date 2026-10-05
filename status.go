package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

// A status message is one the bot pins in a chat and keeps up to date, like
// ruTorrent's status bar: speeds and limits, what rTorrent transferred since
// it started, torrents at work, peers and whether they reach rTorrent's
// port, and free space.

// statusInterval is how often status messages are brought up to date.
const statusInterval = time.Minute

// statusMessage is a chat's status message.
type statusMessage struct {
	Message int `json:"message"`
	Thread  int `json:"thread,omitempty"`
}

func statusKeyboard() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{button("🔄 Refresh", "sr")}}}
}

// status answers /status with a status message, pinned, in place of the
// chat's earlier one; /status off stops it.
func (a *application) status(ctx context.Context, chatID int64, arguments []string) {
	switch strings.ToLower(strings.Join(arguments, " ")) {
	case "":
	case "off":
		if !a.stopStatus(ctx, chatID, "📊 This status is no longer updated. /status shows a new one.") {
			a.send(ctx, chatID, "status: this chat has no status message")
			return
		}
		a.send(ctx, chatID, "The status message is no longer updated or pinned.")
		return
	default:
		a.send(ctx, chatID, "status: use status, or status off")
		return
	}
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	a.stopStatusLocked(ctx, chatID, "📊 This status moved to a newer message.")
	key, err := a.postScreen(ctx, chatID, a.renderStatus(ctx, a.clock()), statusKeyboard(), &screen{status: true})
	if err != nil || key.messageID == 0 {
		return
	}
	thread, _ := ctx.Value(messageThreadIDKey{}).(int)
	if err := a.state.update(func(data *stateData) {
		if data.Status == nil {
			data.Status = make(map[int64]statusMessage)
		}
		data.Status[key.chatID] = statusMessage{Message: key.messageID, Thread: thread}
	}); err != nil {
		a.logger.Printf("[ERROR] saving the status message: %s", err)
	}
	err = a.retryRateLimited(ctx, func() error {
		_, err := a.bot.PinChatMessage(ctx, &telegram.PinChatMessageParams{ChatID: key.chatID, MessageID: key.messageID, DisableNotification: true})
		return err
	})
	if err != nil {
		a.send(ctx, key.chatID, "status: it stays up to date, but could not be pinned: "+redact(a.token, err.Error())+
			". In a group, the bot needs the right to pin messages.")
	}
}

// stopStatus ends a chat's status message, if it has one: it is unpinned and
// left saying note. It reports whether there was one.
func (a *application) stopStatus(ctx context.Context, chatID int64, note string) bool {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	return a.stopStatusLocked(ctx, chatID, note)
}

func (a *application) stopStatusLocked(ctx context.Context, chatID int64, note string) bool {
	var old statusMessage
	var ok bool
	if err := a.state.update(func(data *stateData) {
		old, ok = data.Status[chatID]
		delete(data.Status, chatID)
	}); err != nil {
		a.logger.Printf("[ERROR] saving the status message: %s", err)
	}
	if !ok {
		return false
	}
	a.editScreen(ctx, screenKey{chatID, old.Message}, note, nil) // logs its own failure
	err := a.retryRateLimited(ctx, func() error {
		_, err := a.bot.UnpinChatMessage(ctx, &telegram.UnpinChatMessageParams{ChatID: chatID, MessageID: old.Message})
		return err
	})
	if err != nil {
		a.logger.Printf("[ERROR] Unpin the status message: %s", redact(a.token, err.Error()))
	}
	return true
}

// watchStatus keeps status messages up to date until ctx ends.
func (a *application) watchStatus(ctx context.Context) {
	for {
		a.checkStatus(ctx, a.clock())
		if !waitFor(ctx, statusInterval) {
			return
		}
	}
}

// checkStatus brings every chat's status message up to date. With none, it
// asks rTorrent nothing.
func (a *application) checkStatus(ctx context.Context, now time.Time) {
	var chats map[int64]statusMessage
	a.state.read(func(data *stateData) { chats = maps.Clone(data.Status) })
	if len(chats) == 0 {
		return
	}
	text := a.renderStatus(ctx, now)
	for _, chatID := range slices.Sorted(maps.Keys(chats)) {
		a.updateStatus(ctx, chatID, text)
	}
}

// errNoStatus says a chat has no status message.
var errNoStatus = errors.New("This status is no longer updated. /status shows a new one.")

// updateStatus shows text in a chat's status message. When the message is
// gone, or the bot can no longer post in the chat, the chat's status ends.
func (a *application) updateStatus(ctx context.Context, chatID int64, text string) error {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	var status statusMessage
	var ok bool
	a.state.read(func(data *stateData) { status, ok = data.Status[chatID] })
	if !ok {
		return errNoStatus
	}
	key := screenKey{chatID, status.Message}
	err := a.retryRateLimited(ctx, func() error {
		_, err := a.bot.EditMessageText(ctx, &telegram.EditMessageTextParams{
			ChatID: chatID, MessageID: status.Message, Text: text, ReplyMarkup: statusKeyboard(),
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: telegram.True()},
		})
		return err
	})
	if err != nil && !strings.Contains(err.Error(), "message is not modified") {
		clean := redact(a.token, err.Error())
		if statusGone(err) {
			a.logger.Printf("[INFO] stopped updating the status message in chat %d: %s", chatID, clean)
			if saveErr := a.state.update(func(data *stateData) { delete(data.Status, chatID) }); saveErr != nil {
				a.logger.Printf("[ERROR] saving the status message: %s", saveErr)
			}
			a.screens.forget(key)
		} else {
			a.logger.Printf("[ERROR] Update the status message: %s", clean)
		}
		return errors.New(clean)
	}
	// After a restart, the bot learns again what the message is.
	a.screens.put(key, &screen{status: true})
	return nil
}

// statusGone reports whether err says a status message can no longer be
// edited: it was deleted, its chat became a supergroup, or the bot can no
// longer post in the chat.
func statusGone(err error) bool {
	text := strings.ToLower(err.Error())
	return chatGone(err) || migratedTo(err) != 0 || strings.Contains(text, "message to edit not found") ||
		strings.Contains(text, "message_id_invalid") || strings.Contains(text, "message can't be edited")
}

// pressStatus brings a status message up to date at once.
func (a *application) pressStatus(ctx context.Context, key screenKey) (string, bool) {
	var status statusMessage
	a.state.read(func(data *stateData) { status = data.Status[key.chatID] })
	if status.Message != key.messageID {
		return errNoStatus.Error(), true
	}
	if err := a.updateStatus(ctx, key.chatID, a.renderStatus(ctx, a.clock())); err != nil {
		return err.Error(), true
	}
	return "Updated", false
}

// renderStatus describes rTorrent now.
func (a *application) renderStatus(ctx context.Context, now time.Time) string {
	var text strings.Builder
	text.WriteString("📊 rTorrent status")
	stats, err := a.rtorrent.StatsContext(ctx)
	var down, up uint64
	if err == nil {
		down, up, err = a.rtorrent.SpeedsContext(ctx)
	}
	var torrents rtapi.Torrents
	if err == nil {
		torrents, err = a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
	}
	if err != nil {
		fmt.Fprintf(&text, "\n🔴 rTorrent is not answering: %s\nUpdated %s", err, now.Format("15:04"))
		return text.String()
	}
	limit := func(rate uint64) string {
		if rate == 0 {
			return "no limit"
		}
		return "limit " + formatRate(rate)
	}
	fmt.Fprintf(&text, "\n↓ %s/s · %s · %s this session", formatBytes(down), limit(stats.ThrottleDown), formatBytes(stats.TotalDown))
	fmt.Fprintf(&text, "\n↑ %s/s · %s · %s this session", formatBytes(up), limit(stats.ThrottleUp), formatBytes(stats.TotalUp))

	downloading, uploading, errored := 0, 0, 0
	for _, torrent := range torrents {
		if torrent.State == rtapi.Leeching {
			downloading++
		}
		if torrent.State == rtapi.Error {
			errored++
		}
		if torrent.UpRate > 0 {
			uploading++
		}
	}
	fmt.Fprintf(&text, "\nTorrents: %d · %d downloading · %d uploading", len(torrents), downloading, uploading)
	if errored > 0 {
		fmt.Fprintf(&text, " · %d with errors", errored)
	}

	// A peer can connect to rTorrent only through its port, so one that did
	// shows the port is reachable.
	if peers, err := a.rtorrent.ConnectionsContext(ctx); err == nil {
		switch {
		case peers.Incoming > 0:
			fmt.Fprintf(&text, "\nPeers: %d in, %d out · port %s open ✅", peers.Incoming, peers.Outgoing, stats.Port)
		case peers.Outgoing > 0:
			fmt.Fprintf(&text, "\nPeers: 0 in, %d out · port %s may be closed ⚠️", peers.Outgoing, stats.Port)
		default:
			fmt.Fprintf(&text, "\nPeers: none · port %s", stats.Port)
		}
	}
	if free, err := a.freeSpace(ctx, torrents); err == nil {
		fmt.Fprintf(&text, "\nFree space: %s", formatBytes(free))
	}
	var quiet *quietHours
	a.state.read(func(data *stateData) { quiet = data.Quiet })
	if quiet != nil && quiet.Saved != nil {
		fmt.Fprintf(&text, "\n🌙 Quiet hours until %s", quiet.End)
	}
	fmt.Fprintf(&text, "\nUpdated %s", now.Format("15:04"))
	return text.String()
}
