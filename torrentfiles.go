package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

const (
	filesPageSize = 10
	// filesPerRow is how many file buttons share a row.
	filesPerRow = 5
	// maxUploadSize is Telegram's limit on files a bot sends.
	maxUploadSize = 50_000_000
)

var priorityNames = map[rtapi.FilePriority]string{rtapi.FileSkip: "skip", rtapi.FileNormal: "download", rtapi.FileHigh: "download first"}
var priorityMarks = map[rtapi.FilePriority]string{rtapi.FileSkip: "⬜", rtapi.FileNormal: "✅", rtapi.FileHigh: "⭐"}

// priorityChoice is a priority a file's card offers, with the name its
// button sends.
type priorityChoice struct {
	name, label string
	priority    rtapi.FilePriority
}

var priorityChoices = []priorityChoice{{"skip", "⬜ Skip", rtapi.FileSkip}, {"normal", "✅ Download", rtapi.FileNormal}, {"high", "⭐ First", rtapi.FileHigh}}

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

// fileMatches returns the files whose paths contain every word of filter,
// ignoring case, or all of them for an empty filter.
func fileMatches(files []rtapi.File, filter string) []rtapi.File {
	words := strings.Fields(strings.ToLower(filter))
	if len(words) == 0 {
		return files
	}
	var matches []rtapi.File
	for _, file := range files {
		name := strings.ToLower(file.Path)
		if !slices.ContainsFunc(words, func(word string) bool { return !strings.Contains(name, word) }) {
			matches = append(matches, file)
		}
	}
	return matches
}

// renderFiles renders a page of a torrent's files, or of those matching
// filter. The text describes each file, and a numbered button per file opens
// its card, so the rows stay even however long the names are.
func (a *application) renderFiles(torrent *rtapi.Torrent, files []rtapi.File, filter string, page int, back bool) (string, *models.InlineKeyboardMarkup, int) {
	var text strings.Builder
	var rows [][]models.InlineKeyboardButton
	matches := fileMatches(files, filter)
	pages := max(1, (len(matches)+filesPageSize-1)/filesPageSize)
	page = min(max(page, 0), pages-1)
	fmt.Fprintf(&text, "%s\n", torrent.Name)
	if len(files) == 0 {
		text.WriteString("No files yet; rTorrent is still fetching the torrent's metadata.")
	} else {
		var size, wanted uint64
		selected := 0
		for _, file := range files {
			size += file.Size
			if file.Priority != rtapi.FileSkip {
				selected++
				wanted += file.Size
			}
		}
		fmt.Fprintf(&text, "%d files, %s; downloading %d of them, %s.", len(files), formatBytes(size), selected, formatBytes(wanted))
		if filter != "" {
			fmt.Fprintf(&text, " %d match %q.", len(matches), filter)
		}
		if len(matches) > 0 {
			text.WriteString("\nTap a number to skip, prioritize, or send that file.\n")
		}
	}
	var row []models.InlineKeyboardButton
	for _, file := range matches[page*filesPageSize : min(len(matches), (page+1)*filesPageSize)] {
		fmt.Fprintf(&text, "\n%d. %s %s\n%s · %s", file.Index+1, priorityMarks[file.Priority], file.Path, formatBytes(file.Size), filePercent(file))
		if a.canSend(file) {
			text.WriteString(" · 📥")
		}
		row = append(row, button(priorityMarks[file.Priority]+" "+strconv.Itoa(file.Index+1), "fo:"+strconv.Itoa(file.Index)))
		if len(row) == filesPerRow {
			rows, row = append(rows, row), nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	if pages > 1 {
		var nav []models.InlineKeyboardButton
		if page > 1 {
			nav = append(nav, button("⏮", "pg:0"))
		}
		if page > 0 {
			nav = append(nav, button("◀", "pg:"+strconv.Itoa(page-1)))
		}
		nav = append(nav, button(fmt.Sprintf("%d/%d", page+1, pages), "noop"))
		if page < pages-1 {
			nav = append(nav, button("▶", "pg:"+strconv.Itoa(page+1)))
		}
		if page < pages-2 {
			nav = append(nav, button("⏭", "pg:"+strconv.Itoa(pages-1)))
		}
		rows = append(rows, nav)
	}
	if len(matches) > 0 {
		skip, download := "⬜ Skip all", "✅ Download all"
		if filter != "" {
			skip, download = "⬜ Skip these", "✅ Download these"
		}
		rows = append(rows, []models.InlineKeyboardButton{button(skip, "fp:skip"), button(download, "fp:normal")})
	}
	if back {
		rows = append(rows, []models.InlineKeyboardButton{button("« Back", "back")})
	}
	return text.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}, page
}

// renderFile renders one file's card: what it is, buttons that choose its
// priority, and a 📥 button when it can be sent, or why it cannot.
func (a *application) renderFile(torrent *rtapi.Torrent, file rtapi.File, count int) (string, *models.InlineKeyboardMarkup) {
	var text strings.Builder
	fmt.Fprintf(&text, "%s\nFile %d of %d in %s\n%s · %s done\nPriority: %s %s",
		file.Path, file.Index+1, count, torrent.Name, formatBytes(file.Size), filePercent(file), priorityMarks[file.Priority], priorityNames[file.Priority])
	var choices []models.InlineKeyboardButton
	for _, choice := range priorityChoices {
		label := choice.label
		if choice.priority == file.Priority {
			label = "• " + label
		}
		choices = append(choices, button(label, "fp:"+strconv.Itoa(file.Index)+":"+choice.name))
	}
	rows := [][]models.InlineKeyboardButton{choices}
	switch {
	case a.canSend(file):
		rows = append(rows, []models.InlineKeyboardButton{button("📥 Send", "fg:"+strconv.Itoa(file.Index))})
	case a.dataRoot == "":
		text.WriteString("\nSending files is off; see -data-root.")
	case !file.Complete():
		text.WriteString("\nIt can be sent once it has downloaded.")
	default:
		text.WriteString("\nIt is too large to send: Telegram lets bots send up to 50 MB.")
	}
	rows = append(rows, []models.InlineKeyboardButton{button("« Files", "fl")})
	return text.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// files answers /files HASH [WORDS] with the torrent's files, or those whose
// paths contain every word.
func (a *application) files(ctx context.Context, chatID int64, arguments []string) {
	if len(arguments) == 0 {
		a.send(ctx, chatID, "files: use files HASH [WORDS]")
		return
	}
	torrents, err := a.selected(ctx, chatID, arguments[:1], false)
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
	filter := strings.Join(arguments[1:], " ")
	text, keyboard, page := a.renderFiles(torrent, files, filter, 0, false)
	a.sendScreen(ctx, chatID, text, keyboard, &screen{files: torrent.Hash, filter: filter, page: page})
}

// redrawFiles shows a torrent's files in an existing message: the card of
// the file scr.file names, or else a page of the list.
func (a *application) redrawFiles(ctx context.Context, key screenKey, scr *screen, toast string) (string, bool) {
	torrent, err := a.rtorrent.GetTorrentContext(ctx, scr.files)
	if err != nil {
		return err.Error(), true
	}
	files, err := a.rtorrent.FilesContext(ctx, scr.files)
	if err != nil {
		return err.Error(), true
	}
	next := *scr
	var text string
	var keyboard *models.InlineKeyboardMarkup
	if index := scr.file - 1; index >= 0 && index < len(files) {
		text, keyboard = a.renderFile(torrent, files[index], len(files))
	} else {
		next.file = 0
		text, keyboard, next.page = a.renderFiles(torrent, files, scr.filter, scr.page, scr.parent != nil)
	}
	if err := a.editScreen(ctx, key, text, keyboard); err != nil {
		return err.Error(), true
	}
	a.screens.put(key, &next)
	return toast, false
}

// pressFilePriority sets one file's priority, from its card, or that of
// every file the list shows.
func (a *application) pressFilePriority(ctx context.Context, key screenKey, scr *screen, choice string) (string, bool) {
	files, err := a.rtorrent.FilesContext(ctx, scr.files)
	if err != nil {
		return err.Error(), true
	}
	priorities := make(map[int]rtapi.FilePriority)
	var toast string
	switch choice {
	case "skip", "normal":
		priority := map[string]rtapi.FilePriority{"skip": rtapi.FileSkip, "normal": rtapi.FileNormal}[choice]
		matches := fileMatches(files, scr.filter)
		for _, file := range matches {
			priorities[file.Index] = priority
		}
		toast = fmt.Sprintf("%d files: %s", len(matches), priorityNames[priority])
	default:
		indexText, name, _ := strings.Cut(choice, ":")
		index, err := strconv.Atoi(indexText)
		chosen := slices.IndexFunc(priorityChoices, func(c priorityChoice) bool { return c.name == name })
		if err != nil || index < 0 || index >= len(files) || chosen < 0 {
			return "This button no longer applies.", true
		}
		priorities[index] = priorityChoices[chosen].priority
		toast = path.Base(files[index].Path) + ": " + priorityNames[priorityChoices[chosen].priority]
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
		text, keyboard, page := a.renderFiles(torrent, files, "", 0, false)
		a.sendScreen(ctx, chatID, "Files marked 📥 can be sent.\n\n"+text, keyboard, &screen{files: torrent.Hash, page: page})
		return
	}
	file := files[index]
	if _, err := a.checkUpload(torrent, file); err != nil {
		a.send(ctx, chatID, "get: "+err.Error())
		return
	}
	a.send(ctx, chatID, fmt.Sprintf("Sending %s (%s)…", path.Base(file.Path), formatBytes(file.Size)))
	a.uploadLater(ctx, chatID, torrent, file)
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
	if _, err := a.checkUpload(torrent, files[index]); err != nil {
		return err.Error(), true
	}
	a.uploadLater(ctx, key.chatID, torrent, files[index])
	return "Sending " + path.Base(files[index].Path) + "…", false
}

// uploadLater sends a file in the background, so the bot goes on answering
// while a large file crosses a slow uplink, and says if it fails. Files go
// one at a time.
func (a *application) uploadLater(ctx context.Context, chatID int64, torrent *rtapi.Torrent, file rtapi.File) {
	a.launch(ctx, func(uploadCtx context.Context) {
		a.uploadMu.Lock()
		defer a.uploadMu.Unlock()
		if err := a.upload(uploadCtx, chatID, torrent, file); err != nil && uploadCtx.Err() == nil {
			a.send(uploadCtx, chatID, "get: "+err.Error())
		}
	})
}

// fileLocation returns where a torrent's file is on this machine, as
// rTorrent reports it, or "" when it has not said.
func fileLocation(torrent *rtapi.Torrent, file rtapi.File) string {
	location := dataPath(torrent)
	if location != "" && torrent.MultiFile {
		location = filepath.Join(location, filepath.FromSlash(file.Path))
	}
	return location
}

// checkUpload reports why a file cannot be sent, if it cannot, and returns
// its path inside -data-root.
func (a *application) checkUpload(torrent *rtapi.Torrent, file rtapi.File) (string, error) {
	if a.dataRoot == "" {
		return "", errors.New("sending files is off; set -data-root to the directory on this machine where rTorrent keeps data")
	}
	if !file.Complete() {
		return "", fmt.Errorf("%s has not finished downloading", file.Path)
	}
	if file.Size > maxUploadSize {
		return "", fmt.Errorf("%s is %s, more than the 50 MB Telegram lets bots send", file.Path, formatBytes(file.Size))
	}
	location := fileLocation(torrent, file)
	if location == "" {
		return "", errors.New("rTorrent has not reported where this torrent keeps its data")
	}
	return containedRelative(a.dataRoot, location)
}

// upload sends one finished file of a torrent from disk. The file must be
// inside -data-root, and is opened through it so symbolic links cannot lead
// outside.
func (a *application) upload(ctx context.Context, chatID int64, torrent *rtapi.Torrent, file rtapi.File) error {
	relative, err := a.checkUpload(torrent, file)
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
