package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

func (a *application) launch(ctx context.Context, fn func(context.Context)) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		fn(ctx)
	}()
}

// clock returns the current time; tests replace it with a.now.
func (a *application) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *application) wait(ctx context.Context) bool {
	return waitFor(ctx, a.interval)
}

func waitFor(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func chunkMessage(text string) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	chunks := make([]string, 0, (len(runes)+maxTelegramMessage-1)/maxTelegramMessage)
	for len(runes) > maxTelegramMessage {
		cut := maxTelegramMessage
		for i := cut - 1; i > 0; i-- {
			if runes[i] == '\n' {
				cut = i + 1
				break
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) != 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

func (a *application) send(ctx context.Context, chatID int64, text string) (int, error) {
	_, messageID, err := a.sendText(ctx, chatID, text)
	return messageID, err
}

// sendText is send that also returns the chat the text went to, which is
// not chatID once a group has become a supergroup.
func (a *application) sendText(ctx context.Context, chatID int64, text string) (int64, int, error) {
	chunks := chunkMessage(text)
	if len(chunks) == 0 {
		return chatID, 0, errors.New("refusing to send an empty message")
	}
	messageThreadID, _ := ctx.Value(messageThreadIDKey{}).(int)
	_, _ = a.bot.SendChatAction(ctx, &telegram.SendChatActionParams{
		ChatID: chatID, MessageThreadID: messageThreadID, Action: models.ChatActionTyping,
	})
	if len(chunks) > maxMessageChunks {
		chatID, err := a.sendFile(ctx, chatID, text)
		return chatID, 0, err
	}
	messageID := 0
	for _, chunk := range chunks {
		var message *models.Message
		var err error
		chatID, err = a.post(ctx, chatID, func(chatID int64, thread int) (err error) {
			message, err = a.bot.SendMessage(ctx, &telegram.SendMessageParams{
				ChatID:             chatID,
				MessageThreadID:    thread,
				Text:               chunk,
				LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: telegram.True()},
			})
			return err
		})
		if err != nil {
			a.logger.Printf("[ERROR] Send: %s", err)
			return chatID, 0, err
		}
		messageID = message.ID
	}
	if len(chunks) != 1 {
		return chatID, 0, nil
	}
	return chatID, messageID, nil
}

// sendFile attaches text as a file, for replies too long to read as messages.
func (a *application) sendFile(ctx context.Context, chatID int64, text string) (int64, error) {
	chatID, err := a.post(ctx, chatID, func(chatID int64, thread int) error {
		_, err := a.bot.SendDocument(ctx, &telegram.SendDocumentParams{
			ChatID:          chatID,
			MessageThreadID: thread,
			Document:        &models.InputFileUpload{Filename: "rtelegram.txt", Data: strings.NewReader(text)},
			Caption:         "This reply is too long for chat messages, so it is attached as a file.",
		})
		return err
	})
	if err != nil {
		a.logger.Printf("[ERROR] Send file: %s", err)
	}
	return chatID, err
}

// telegramError is a failed Telegram request, with the bot token kept out of
// its text. It unwraps to the library's error, so that failures can be told
// apart.
type telegramError struct {
	text string
	err  error
}

func (e *telegramError) Error() string { return e.text }
func (e *telegramError) Unwrap() error { return e.err }

func (a *application) redactError(err error) error {
	return &telegramError{redact(a.token, err.Error()), err}
}

// post makes request, which posts in a chat: the chat it is given, in the
// forum topic of ctx. Rate limits are waited out. When Telegram says a group
// has become a supergroup, which has an ID of its own, what the bot keeps
// for the chat moves to that ID, and the request is made there; when the
// topic is gone, the chat's settings forget it, and the request is made in
// the chat itself. post returns the chat it posted in.
func (a *application) post(ctx context.Context, chatID int64, request func(chatID int64, thread int) error) (int64, error) {
	thread, _ := ctx.Value(messageThreadIDKey{}).(int)
	for attempt := 1; ; attempt++ {
		err := a.retryRateLimited(ctx, func() error { return request(chatID, thread) })
		if err == nil {
			return chatID, nil
		}
		if attempt < 3 {
			if to := migratedTo(err); to != 0 && to != chatID {
				a.logger.Printf("[INFO] Chat %d became chat %d, so its settings move there", chatID, to)
				a.moveChat(chatID, to)
				chatID = to
				continue
			}
			if thread != 0 && strings.Contains(strings.ToLower(err.Error()), "message thread not found") {
				a.logger.Printf("[INFO] Topic %d of chat %d is gone, so its messages go to the chat itself", thread, chatID)
				a.forgetTopic(chatID, thread)
				thread = 0
				continue
			}
		}
		return chatID, a.redactError(err)
	}
}

// migrateToChatID finds the new chat ID in Telegram's answer to a request
// to a group that has become a supergroup.
var migrateToChatID = regexp.MustCompile(`"migrate_to_chat_id":\s*(-?\d+)`)

// migratedTo returns the chat that err says a group has become, or 0.
func migratedTo(err error) int64 {
	var migrate *telegram.MigrateError
	if errors.As(err, &migrate) {
		return int64(migrate.MigrateToChatID)
	}
	// Where int has 32 bits, the library cannot decode a supergroup's ID,
	// and its error quotes Telegram's answer instead.
	if match := migrateToChatID.FindStringSubmatch(err.Error()); match != nil {
		to, _ := strconv.ParseInt(match[1], 10, 64)
		return to
	}
	return 0
}

// chatGone reports whether err says the bot can no longer post in a chat:
// it was blocked or removed, or the chat no longer exists.
func chatGone(err error) bool {
	return errors.Is(err, telegram.ErrorForbidden) || strings.Contains(strings.ToLower(err.Error()), "chat not found")
}

// moveChat moves what the bot keeps for chat from to chat to, which from has
// become. What to already has is kept.
func (a *application) moveChat(from, to int64) {
	err := a.state.update(func(data *stateData) {
		moveKey(data.Notify, from, to)
		moveKey(data.Digest, from, to)
		moveKey(data.Sorts, from, to)
		for i := range data.Watch {
			if data.Watch[i].ChatID == from {
				data.Watch[i].ChatID = to
			}
		}
	})
	if err != nil {
		a.logger.Printf("[ERROR] Saving that chat %d became chat %d: %s", from, to, err)
	}
}

func moveKey[V any](m map[int64]V, from, to int64) {
	if value, ok := m[from]; ok {
		delete(m, from)
		if _, taken := m[to]; !taken {
			m[to] = value
		}
	}
}

// forgetTopic makes what posted in a chat's forum topic, which is gone,
// post in the chat itself.
func (a *application) forgetTopic(chatID int64, thread int) {
	err := a.state.update(func(data *stateData) {
		if settings, ok := data.Notify[chatID]; ok && settings.Thread == thread {
			settings.Thread = 0
			data.Notify[chatID] = settings
		}
		if settings, ok := data.Digest[chatID]; ok && settings.Thread == thread {
			settings.Thread = 0
			data.Digest[chatID] = settings
		}
		for i := range data.Watch {
			if data.Watch[i].ChatID == chatID && data.Watch[i].Thread == thread {
				data.Watch[i].Thread = 0
			}
		}
	})
	if err != nil {
		a.logger.Printf("[ERROR] Saving that topic %d of chat %d is gone: %s", thread, chatID, err)
	}
}

// retryRateLimited repeats a Telegram request while Telegram answers 429 Too
// Many Requests, waiting as long as Telegram asks.
func (a *application) retryRateLimited(ctx context.Context, request func() error) error {
	for attempt := 1; ; attempt++ {
		err := request()
		var limited *telegram.TooManyRequestsError
		if !errors.As(err, &limited) || attempt == maxTelegramAttempts {
			return err
		}
		wait := time.Duration(limited.RetryAfter) * time.Second
		if wait > maxRetryAfter {
			return err
		}
		a.logger.Printf("[WARN] Telegram rate limit; retrying in %s", wait)
		if !waitFor(ctx, wait) {
			return err
		}
	}
}

func (a *application) edit(ctx context.Context, chatID int64, messageID int, text string) error {
	if messageID == 0 || text == "" || len([]rune(text)) > maxTelegramMessage {
		return errors.New("message cannot be edited")
	}
	_, err := a.bot.EditMessageText(ctx, &telegram.EditMessageTextParams{
		ChatID:             chatID,
		MessageID:          messageID,
		Text:               text,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: telegram.True()},
	})
	if err != nil {
		clean := redact(a.token, err.Error())
		a.logger.Printf("[ERROR] Edit: %s", clean)
		return errors.New(clean)
	}
	return nil
}

func (a *application) editChanged(ctx context.Context, chatID int64, messageID int, current *string, next string) error {
	if next == *current {
		return nil
	}
	if err := a.edit(ctx, chatID, messageID, next); err != nil {
		return err
	}
	*current = next
	return nil
}

func (a *application) getVersion(ctx context.Context, chatID int64) {
	a.send(ctx, chatID, fmt.Sprintf("rTorrent/libtorrent: %s\nrtelegram: %s", a.rtorrent.Version, version))
}

// torrents lists every torrent in chatID's sort order, without trackers:
// fetching them is a call per torrent, which large libraries feel.
func (a *application) torrents(ctx context.Context, chatID int64) (rtapi.Torrents, error) {
	torrents, err := a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
	if err != nil {
		return nil, err
	}
	var sorting rtapi.Sorting
	a.state.read(func(data *stateData) { sorting = data.Sorts[chatID] })
	switch sorting {
	case rtapi.ByAge:
		// Age is reset when rTorrent restarts, so sort by when torrents
		// were added.
		slices.SortStableFunc(torrents, func(x, y *rtapi.Torrent) int { return newestFirst(y, x) })
	case rtapi.ByAgeRev:
		slices.SortStableFunc(torrents, newestFirst)
	default:
		torrents.Sort(sorting)
	}
	return torrents, nil
}
