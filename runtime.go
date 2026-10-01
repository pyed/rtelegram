package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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
	torrents = slices.Clone(torrents)
	a.sortMu.RLock()
	preference, ok := a.sorts[chatID]
	a.sortMu.RUnlock()
	if !ok {
		return torrents, nil
	}
	compare := func(left, right *rtapi.Torrent) int {
		switch preference.key {
		case "name":
			return strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
		case "downrate":
			return cmp.Compare(left.DownRate, right.DownRate)
		case "uprate":
			return cmp.Compare(left.UpRate, right.UpRate)
		case "size":
			return cmp.Compare(left.Size, right.Size)
		case "ratio":
			return cmp.Compare(left.Ratio, right.Ratio)
		case "age":
			return cmp.Compare(left.Age, right.Age)
		default:
			return cmp.Compare(left.UpTotal, right.UpTotal)
		}
	}
	slices.SortStableFunc(torrents, func(left, right *rtapi.Torrent) int {
		order := compare(left, right)
		if preference.reverse {
			return -order
		}
		return order
	})
	return torrents, nil
}

func (a *application) watchCompletedLog(ctx context.Context, path string) {
	a.watchCompletedLogEvery(ctx, path, 500*time.Millisecond, 5*time.Second)
}

func (a *application) watchCompletedLogEvery(ctx context.Context, path string, pollInterval, retryInterval time.Duration) {
	var file *os.File
	var reader *bufio.Reader
	var fileInfo os.FileInfo
	var offset int64
	var pending string
	firstOpen := true
	lastOpenError := ""

	closeFile := func() {
		if file != nil {
			_ = file.Close()
		}
		file, reader, fileInfo = nil, nil, nil
		offset, pending = 0, ""
	}
	defer closeFile()
	open := func(atEnd bool) error {
		closeFile()
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if atEnd {
			offset, err = f.Seek(0, io.SeekEnd)
			if err != nil {
				f.Close()
				return err
			}
		}
		file, reader, fileInfo = f, bufio.NewReader(f), info
		return nil
	}

	for {
		if file == nil {
			if err := open(firstOpen); err != nil {
				if firstOpen && errors.Is(err, os.ErrNotExist) {
					firstOpen = false
				}
				if message := err.Error(); message != lastOpenError {
					a.logger.Printf("[ERROR] tailing completed torrents log: %s", err)
					lastOpenError = message
				}
				if !waitFor(ctx, retryInterval) {
					return
				}
				continue
			}
			lastOpenError = ""
			firstOpen = false
		}
		fragment, err := reader.ReadString('\n')
		if fragment != "" {
			offset += int64(len(fragment))
			pending += fragment
			for {
				newline := strings.IndexByte(pending, '\n')
				if newline < 0 {
					break
				}
				line := strings.TrimSpace(pending[:newline])
				pending = pending[newline+1:]
				if line != "" {
					if _, sendErr := a.send(ctx, a.notifyChatID, "Completed: "+line); sendErr != nil && ctx.Err() == nil {
						a.logger.Printf("[ERROR] completion notification: %s", redact(a.token, sendErr.Error()))
					}
				}
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			a.logger.Printf("[ERROR] tailing completed torrents log: %s", err)
			closeFile()
			if !waitFor(ctx, retryInterval) {
				return
			}
			continue
		}
		current, statErr := os.Stat(path)
		if statErr != nil || !os.SameFile(fileInfo, current) || current.Size() < offset {
			closeFile()
			continue
		}
		if !waitFor(ctx, pollInterval) {
			return
		}
	}
}
