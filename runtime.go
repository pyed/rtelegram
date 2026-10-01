package main

import (
	"context"
	"errors"
	"fmt"
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
	chunks := chunkMessage(text)
	if len(chunks) == 0 {
		return 0, errors.New("refusing to send an empty message")
	}
	messageThreadID, _ := ctx.Value(messageThreadIDKey{}).(int)
	_, _ = a.bot.SendChatAction(ctx, &telegram.SendChatActionParams{
		ChatID: chatID, MessageThreadID: messageThreadID, Action: models.ChatActionTyping,
	})
	if len(chunks) > maxMessageChunks {
		return 0, a.sendFile(ctx, chatID, messageThreadID, text)
	}
	messageID := 0
	for _, chunk := range chunks {
		var message *models.Message
		err := a.retryRateLimited(ctx, func() (err error) {
			message, err = a.bot.SendMessage(ctx, &telegram.SendMessageParams{
				ChatID:             chatID,
				MessageThreadID:    messageThreadID,
				Text:               chunk,
				LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: telegram.True()},
			})
			return err
		})
		if err != nil {
			clean := redact(a.token, err.Error())
			a.logger.Printf("[ERROR] Send: %s", clean)
			return 0, errors.New(clean)
		}
		messageID = message.ID
	}
	if len(chunks) != 1 {
		return 0, nil
	}
	return messageID, nil
}

// sendFile attaches text as a file, for replies too long to read as messages.
func (a *application) sendFile(ctx context.Context, chatID int64, messageThreadID int, text string) error {
	err := a.retryRateLimited(ctx, func() error {
		_, err := a.bot.SendDocument(ctx, &telegram.SendDocumentParams{
			ChatID:          chatID,
			MessageThreadID: messageThreadID,
			Document:        &models.InputFileUpload{Filename: "rtelegram.txt", Data: strings.NewReader(text)},
			Caption:         "This reply is too long for chat messages, so it is attached as a file.",
		})
		return err
	})
	if err != nil {
		clean := redact(a.token, err.Error())
		a.logger.Printf("[ERROR] Send file: %s", clean)
		return errors.New(clean)
	}
	return nil
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

func (a *application) torrents(ctx context.Context, chatID int64) (rtapi.Torrents, error) {
	torrents, err := a.rtorrent.TorrentsContext(ctx)
	if err != nil {
		return nil, err
	}
	var sorting rtapi.Sorting
	a.state.read(func(data *stateData) { sorting = data.Sorts[chatID] })
	torrents.Sort(sorting)
	return torrents, nil
}
