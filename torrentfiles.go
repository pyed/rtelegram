package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

const (
	filesPageSize = 10
	// maxUploadSize is Telegram's limit on files a bot sends.
	maxUploadSize = 50_000_000
)

var priorityNames = map[rtapi.FilePriority]string{rtapi.FileSkip: "skip", rtapi.FileNormal: "normal", rtapi.FileHigh: "high"}
var priorityMarks = map[rtapi.FilePriority]string{rtapi.FileSkip: "⬜ ", rtapi.FileNormal: "✅ ", rtapi.FileHigh: "⭐ "}

func filePercent(file rtapi.File) string {
	if file.Chunks == 0 {
		return "100%"
	}
	return fmt.Sprintf("%.0f%%", float64(file.CompletedChunks)*100/float64(file.Chunks))
}

// canSend reports whether /get could send a file.
func (a *application) canSend(file rtapi.File) bool {
	return a.dataRoot != "" && file.Complete() && file.Size <= maxUploadSize
}

// renderFiles renders a page of a torrent's files with a button per file that
// cycles its priority, and a 📥 button for files that can be sent.
func (a *application) renderFiles(torrent *rtapi.Torrent, files []rtapi.File, page int, back bool) (string, *models.InlineKeyboardMarkup, int) {
	var text strings.Builder
	var rows [][]models.InlineKeyboardButton
	pages := max(1, (len(files)+filesPageSize-1)/filesPageSize)
	page = min(max(page, 0), pages-1)
	fmt.Fprintf(&text, "%s\n", torrent.Name)
	if len(files) == 0 {
		text.WriteString("No files yet; rTorrent is still fetching the torrent's metadata.")
	} else {
		fmt.Fprintf(&text, "%d files. Tap one to cycle skip, normal, high.\n", len(files))
	}
	for _, file := range files[page*filesPageSize : min(len(files), (page+1)*filesPageSize)] {
		fmt.Fprintf(&text, "\n%d. %s\n%s, %s, %s", file.Index+1, file.Path,
			formatBytes(file.Size), filePercent(file), priorityNames[file.Priority])
		row := []models.InlineKeyboardButton{button(priorityMarks[file.Priority]+buttonName(path.Base(file.Path)), "fp:"+strconv.Itoa(file.Index))}
		if a.canSend(file) {
			row = append(row, button("📥", "fg:"+strconv.Itoa(file.Index)))
		}
		rows = append(rows, row)
	}
	if pages > 1 {
		fmt.Fprintf(&text, "\n\nPage %d of %d", page+1, pages)
		var nav []models.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, button("◀", "pg:"+strconv.Itoa(page-1)))
		}
		nav = append(nav, button(fmt.Sprintf("%d/%d", page+1, pages), "noop"))
		if page < pages-1 {
			nav = append(nav, button("▶", "pg:"+strconv.Itoa(page+1)))
		}
		rows = append(rows, nav)
	}
	if len(files) > 0 {
		rows = append(rows, []models.InlineKeyboardButton{button("⬜ Skip all", "fp:skip"), button("✅ Download all", "fp:normal")})
	}
	if back {
		rows = append(rows, []models.InlineKeyboardButton{button("« Back", "back")})
	}
	return text.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}, page
}

// files answers /files HASH with the torrent's files.
func (a *application) files(ctx context.Context, chatID int64, references []string) {
	if len(references) != 1 {
		a.send(ctx, chatID, "files: use files HASH")
		return
	}
	torrents, err := a.selected(ctx, chatID, references, false)
	if err != nil {
		a.send(ctx, chatID, "files: "+err.Error())
		return
	}
	torrent := torrents[0]
	files, err := a.rtorrent.FilesContext(ctx, torrent.Hash)
	if err != nil {
		a.send(ctx, chatID, "files: "+err.Error())
		return
	}
	text, keyboard, page := a.renderFiles(torrent, files, 0, false)
	a.sendScreen(ctx, chatID, text, keyboard, &screen{files: torrent.Hash, page: page})
}

// redrawFiles shows a page of a torrent's files in an existing message.
func (a *application) redrawFiles(ctx context.Context, key screenKey, scr *screen, toast string) (string, bool) {
	torrent, err := a.rtorrent.GetTorrentContext(ctx, scr.files)
	if err != nil {
		return err.Error(), true
	}
	files, err := a.rtorrent.FilesContext(ctx, scr.files)
	if err != nil {
		return err.Error(), true
	}
	text, keyboard, page := a.renderFiles(torrent, files, scr.page, scr.parent != nil)
	if err := a.editScreen(ctx, key, text, keyboard); err != nil {
		return err.Error(), true
	}
	next := *scr
	next.page = page
	a.screens.put(key, &next)
	return toast, false
}

// pressFilePriority cycles one file's priority, or sets every file's.
func (a *application) pressFilePriority(ctx context.Context, key screenKey, scr *screen, choice string) (string, bool) {
	files, err := a.rtorrent.FilesContext(ctx, scr.files)
	if err != nil {
		return err.Error(), true
	}
	priorities := make(map[int]rtapi.FilePriority)
	var toast string
	switch choice {
	case "skip", "normal":
		for _, file := range files {
			priorities[file.Index] = map[string]rtapi.FilePriority{"skip": rtapi.FileSkip, "normal": rtapi.FileNormal}[choice]
		}
		toast = "Every file: " + choice
	default:
		index, err := strconv.Atoi(choice)
		if err != nil || index < 0 || index >= len(files) {
			return "This button no longer applies.", true
		}
		next := (files[index].Priority + 1) % 3
		priorities[index] = next
		toast = path.Base(files[index].Path) + ": " + priorityNames[next]
	}
	if err := a.rtorrent.SetFilePrioritiesContext(ctx, scr.files, priorities); err != nil {
		return err.Error(), true
	}
	return a.redrawFiles(ctx, key, scr, toast)
}

// get answers /get HASH [N] by sending a finished file. A single-file
// torrent needs no N; a multi-file one shows its files to choose from.
func (a *application) get(ctx context.Context, chatID int64, arguments []string) {
	if len(arguments) == 0 || len(arguments) > 2 {
		a.send(ctx, chatID, "get: use get HASH [FILE NUMBER]")
		return
	}
	torrents, err := a.selected(ctx, chatID, arguments[:1], false)
	if err != nil {
		a.send(ctx, chatID, "get: "+err.Error())
		return
	}
	torrent := torrents[0]
	files, err := a.rtorrent.FilesContext(ctx, torrent.Hash)
	if err != nil {
		a.send(ctx, chatID, "get: "+err.Error())
		return
	}
	index := 0
	switch {
	case len(arguments) == 2:
		number, err := strconv.Atoi(arguments[1])
		if err != nil || number < 1 || number > len(files) {
			a.send(ctx, chatID, fmt.Sprintf("get: file number must be between 1 and %d", len(files)))
			return
		}
		index = number - 1
	case len(files) != 1:
		text, keyboard, page := a.renderFiles(torrent, files, 0, false)
		a.sendScreen(ctx, chatID, "Choose a file with 📥.\n\n"+text, keyboard, &screen{files: torrent.Hash, page: page})
		return
	}
	if err := a.upload(ctx, chatID, torrent, files[index]); err != nil {
		a.send(ctx, chatID, "get: "+err.Error())
	}
}

// pressGet sends the file a 📥 button names.
func (a *application) pressGet(ctx context.Context, key screenKey, scr *screen, choice string) (string, bool) {
	torrent, err := a.rtorrent.GetTorrentContext(ctx, scr.files)
	if err != nil {
		return err.Error(), true
	}
	files, err := a.rtorrent.FilesContext(ctx, scr.files)
	if err != nil {
		return err.Error(), true
	}
	index, err := strconv.Atoi(choice)
	if err != nil || index < 0 || index >= len(files) {
		return "This button no longer applies.", true
	}
	if err := a.upload(ctx, key.chatID, torrent, files[index]); err != nil {
		return err.Error(), true
	}
	return "Sent " + path.Base(files[index].Path), false
}

// upload sends one finished file of a torrent from disk. The file must be
// inside -data-root, and is opened through it so symbolic links cannot lead
// outside.
func (a *application) upload(ctx context.Context, chatID int64, torrent *rtapi.Torrent, file rtapi.File) error {
	if a.dataRoot == "" {
		return errors.New("sending files is off; set -data-root to the directory on this machine where rTorrent keeps data")
	}
	if !file.Complete() {
		return fmt.Errorf("%s has not finished downloading", file.Path)
	}
	if file.Size > maxUploadSize {
		return fmt.Errorf("%s is %s, more than the 50 MB Telegram lets bots send", file.Path, formatBytes(file.Size))
	}
	location := dataPath(torrent)
	if location == "" {
		return errors.New("rTorrent has not reported where this torrent keeps its data")
	}
	if torrent.MultiFile {
		location = filepath.Join(location, filepath.FromSlash(file.Path))
	}
	relative, err := containedRelative(a.dataRoot, location)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(a.dataRoot)
	if err != nil {
		return fmt.Errorf("open data root: %w", err)
	}
	defer root.Close()
	info, err := root.Stat(relative)
	if err != nil {
		return fmt.Errorf("read %s: %w", file.Path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxUploadSize {
		return fmt.Errorf("%s is not a regular file under 50 MB", file.Path)
	}
	messageThreadID, _ := ctx.Value(messageThreadIDKey{}).(int)
	err = a.retryRateLimited(ctx, func() error {
		content, err := root.Open(relative)
		if err != nil {
			return err
		}
		defer content.Close()
		_, err = a.bot.SendDocument(ctx, &telegram.SendDocumentParams{
			ChatID:          chatID,
			MessageThreadID: messageThreadID,
			Document:        &models.InputFileUpload{Filename: path.Base(file.Path), Data: content},
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
