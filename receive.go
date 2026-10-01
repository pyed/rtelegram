package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

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
	hash, err := torrentInfoHash(data)
	if err != nil {
		a.send(ctx, chatID, fmt.Sprintf("receiver: %s is not a valid torrent file: %s", document.FileName, err))
		return
	}
	before, err := a.loadedHashes(ctx)
	if err != nil {
		a.send(ctx, chatID, "receiver: "+err.Error())
		return
	}
	if before[hash] {
		a.send(ctx, chatID, "receiver: "+document.FileName+" is already loaded")
		return
	}
	directory, label := processOptions(caption)
	if directory, err = a.downloadDirectory(ctx, directory); err != nil {
		a.send(ctx, chatID, "receiver: "+err.Error())
		return
	}
	options := &rtapi.DotTorrentWithOptions{Name: document.FileName, Dir: directory, Label: label, Stopped: a.addStopped}
	if err := a.rtorrent.DownloadRawContext(ctx, data, options); err != nil {
		a.logger.Printf("add uploaded torrent: %s", redact(a.token, err.Error()))
		a.send(ctx, chatID, "receiver: "+redact(a.token, err.Error()))
		return
	}
	a.launch(ctx, func(confirmCtx context.Context) {
		a.confirmAdded(confirmCtx, chatID, "receiver", document.FileName, hash, before)
	})
}

// torrentInfoHash returns the upper-case hex SHA-1 of a .torrent file's info
// dictionary, which is the hash rTorrent lists the torrent under.
func torrentInfoHash(data []byte) (string, error) {
	if len(data) == 0 || data[0] != 'd' {
		return "", errors.New("not a bencoded dictionary")
	}
	for i := 1; i < len(data) && data[i] != 'e'; {
		key, valueStart, err := bencodeString(data, i)
		if err != nil {
			return "", err
		}
		valueEnd, err := bencodeEnd(data, valueStart, 0)
		if err != nil {
			return "", err
		}
		if key == "info" {
			if data[valueStart] != 'd' {
				return "", errors.New("info is not a dictionary")
			}
			sum := sha1.Sum(data[valueStart:valueEnd])
			return strings.ToUpper(hex.EncodeToString(sum[:])), nil
		}
		i = valueEnd
	}
	return "", errors.New("no info dictionary")
}

// bencodeString decodes the byte string at data[i:] and returns it with the
// index just past it.
func bencodeString(data []byte, i int) (string, int, error) {
	colon := bytes.IndexByte(data[i:], ':')
	if colon <= 0 {
		return "", 0, errors.New("malformed string")
	}
	length, err := strconv.Atoi(string(data[i : i+colon]))
	start := i + colon + 1
	if err != nil || length < 0 || length > len(data)-start {
		return "", 0, errors.New("malformed string")
	}
	return string(data[start : start+length]), start + length, nil
}

// bencodeEnd returns the index just past the bencoded value at data[i:].
func bencodeEnd(data []byte, i, depth int) (int, error) {
	if i >= len(data) {
		return 0, errors.New("truncated")
	}
	if depth > 64 {
		return 0, errors.New("nested too deeply")
	}
	switch c := data[i]; {
	case c == 'i':
		end := bytes.IndexByte(data[i:], 'e')
		if end < 0 {
			return 0, errors.New("malformed integer")
		}
		return i + end + 1, nil
	case c == 'l' || c == 'd':
		for i++; i < len(data) && data[i] != 'e'; {
			var err error
			if i, err = bencodeEnd(data, i, depth+1); err != nil {
				return 0, err
			}
		}
		if i >= len(data) {
			return 0, errors.New("truncated")
		}
		return i + 1, nil
	case c >= '0' && c <= '9':
		_, end, err := bencodeString(data, i)
		return end, err
	}
	return 0, fmt.Errorf("unexpected byte %q", data[i])
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

// downloadDirectory confines a caption's download directory to the download
// root, so a caption cannot make rTorrent write anywhere else on its host. The
// root is -download-root, or rTorrent's default directory when that is unset.
func (a *application) downloadDirectory(ctx context.Context, requested string) (string, error) {
	if requested == "" {
		return "", nil
	}
	root := a.downloadRoot
	if root == "" {
		stats, err := a.rtorrent.StatsContext(ctx)
		if err != nil {
			return "", err
		}
		root = stats.Directory
	}
	return confineDirectory(root, requested)
}

// confineDirectory places a relative directory under root and accepts an
// absolute one only inside root. Paths are slash-separated because rTorrent
// interprets them on its own host.
func confineDirectory(root, requested string) (string, error) {
	if !path.IsAbs(root) {
		return "", fmt.Errorf("download directories are disabled: the download root %q is not absolute; set -download-root", root)
	}
	root = path.Clean(root)
	directory := requested
	if !path.IsAbs(directory) {
		directory = path.Join(root, directory)
	}
	directory = path.Clean(directory)
	if root != "/" && directory != root && !strings.HasPrefix(directory, root+"/") {
		return "", fmt.Errorf("%s is outside the download root %s; set -download-root to allow it", requested, root)
	}
	return directory, nil
}

// processOptions reads an upload caption's d=DIRECTORY and l=LABEL options.
// As in earlier versions, one other word is the directory if it contains a
// slash and the label otherwise. Two or more other words are a note, and are
// ignored rather than becoming a label.
func processOptions(caption string) (directory, label string) {
	var words []string
	for _, field := range strings.Fields(caption) {
		if value, ok := strings.CutPrefix(field, "d="); ok {
			directory = value
		} else if value, ok := strings.CutPrefix(field, "l="); ok {
			label = value
		} else {
			words = append(words, field)
		}
	}
	if len(words) == 1 {
		switch {
		case strings.ContainsAny(words[0], `/\`):
			if directory == "" {
				directory = words[0]
			}
		case label == "":
			label = words[0]
		}
	}
	return directory, label
}
