package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

type rawDownloader interface {
	DownloadRaw(data []byte, options *rtapi.DotTorrentWithOptions) error
}

func downloadRaw(target any, data []byte, options *rtapi.DotTorrentWithOptions) error {
	downloader, ok := target.(rawDownloader)
	if !ok {
		return errors.New("raw torrent uploads require a newer rtapi release")
	}
	return downloader.DownloadRaw(data, options)
}

func (a *application) receiveTorrent(ctx context.Context, chatID int64, message *models.Message, caption string) {
	if message == nil || message.Document == nil {
		return
	}
	document := message.Document
	if document.FileSize > maxTorrentFileSize {
		a.send(ctx, chatID, fmt.Sprintf("receiver: torrent file exceeds the %s limit", formatBytes(maxTorrentFileSize)))
		return
	}
	data, err := a.downloadTelegramFile(ctx, document.FileID)
	if err != nil {
		a.send(ctx, chatID, "receiver: "+redact(a.token, err.Error()))
		return
	}
	directory, label := processOptions(caption)
	options := &rtapi.DotTorrentWithOptions{Name: document.FileName, Dir: directory, Label: label}
	if err := downloadRaw(a.rtorrent, data, options); err != nil {
		a.logger.Printf("add uploaded torrent: %s", redact(a.token, err.Error()))
		a.send(ctx, chatID, "receiver: "+redact(a.token, err.Error()))
		return
	}
	a.send(ctx, chatID, "Added: "+document.FileName)
}

func (a *application) downloadTelegramFile(ctx context.Context, fileID string) ([]byte, error) {
	file, err := a.bot.GetFile(ctx, &telegram.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get Telegram file: %s", redact(a.token, err.Error()))
	}
	if file.FileSize > maxTorrentFileSize {
		return nil, fmt.Errorf("torrent file exceeds the %s limit", formatBytes(maxTorrentFileSize))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.bot.FileDownloadLink(file), nil)
	if err != nil {
		return nil, errors.New("create Telegram file request")
	}
	response, err := a.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download Telegram file: %s", redact(a.token, err.Error()))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download Telegram file: Telegram returned %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTorrentFileSize+1))
	if err != nil {
		return nil, errors.New("read Telegram file response")
	}
	if len(data) > maxTorrentFileSize {
		return nil, fmt.Errorf("torrent file exceeds the %s limit", formatBytes(maxTorrentFileSize))
	}
	if len(data) == 0 {
		return nil, errors.New("telegram returned an empty torrent file")
	}
	return data, nil
}

func processOptions(options string) (directory, label string) {
	for _, option := range strings.Fields(options) {
		switch {
		case strings.HasPrefix(option, "d="):
			directory = strings.TrimPrefix(option, "d=")
		case strings.HasPrefix(option, "l="):
			label = strings.TrimPrefix(option, "l=")
		case strings.ContainsAny(option, "/\\"):
			directory = option
		default:
			label = option
		}
	}
	return directory, label
}
