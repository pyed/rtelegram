package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pyed/rtapi"
)

type metadataDeleter interface {
	DeleteMetadata(...*rtapi.Torrent) error
}

func deleteMetadata(target any, torrents ...*rtapi.Torrent) error {
	deleter, ok := target.(metadataDeleter)
	if !ok {
		return errors.New("safe metadata deletion requires a newer rtapi release")
	}
	return deleter.DeleteMetadata(torrents...)
}

func mutationError(action string, count int, err error) string {
	message := action + ": " + err.Error()
	if count > 1 {
		message += "; rTorrent may have applied the operation to some torrents, so refresh before retrying"
	}
	return message
}

func (a *application) add(ctx context.Context, chatID int64, sources []string, filename string) {
	if len(sources) == 0 {
		a.send(ctx, chatID, "add: needs at least one URL")
		return
	}
	for _, source := range sources {
		if err := a.rtorrent.Download(source); err != nil {
			a.logger.Printf("add: %s", err)
			a.send(ctx, chatID, "add: "+err.Error())
			continue
		}
		name := filename
		if name == "" {
			name = filepath.Base(source)
		}
		a.send(ctx, chatID, "Added: "+name)
	}
}

func (a *application) selected(chatID int64, references []string, allowAll bool) (rtapi.Torrents, error) {
	torrents, err := a.torrents(chatID)
	if err != nil {
		return nil, err
	}
	return selectTorrents(torrents, references, allowAll)
}

func (a *application) start(ctx context.Context, chatID int64, references []string) {
	torrents, err := a.selected(chatID, references, true)
	if err != nil {
		a.send(ctx, chatID, "start: "+err.Error())
		return
	}
	if err := a.rtorrent.Start(torrents...); err != nil {
		a.logger.Printf("start: %s", err)
		a.send(ctx, chatID, mutationError("start", len(torrents), err))
		return
	}
	a.send(ctx, chatID, "Started: "+pluralNames(torrents))
}

func (a *application) stop(ctx context.Context, chatID int64, references []string) {
	torrents, err := a.selected(chatID, references, true)
	if err != nil {
		a.send(ctx, chatID, "stop: "+err.Error())
		return
	}
	if err := a.rtorrent.Stop(torrents...); err != nil {
		a.logger.Printf("stop: %s", err)
		a.send(ctx, chatID, mutationError("stop", len(torrents), err))
		return
	}
	a.send(ctx, chatID, "Stopped: "+pluralNames(torrents))
}

func (a *application) check(ctx context.Context, chatID int64, references []string) {
	torrents, err := a.selected(chatID, references, true)
	if err != nil {
		a.send(ctx, chatID, "check: "+err.Error())
		return
	}
	if err := a.rtorrent.Check(torrents...); err != nil {
		a.logger.Printf("check: %s", err)
		a.send(ctx, chatID, mutationError("check", len(torrents), err))
		return
	}
	a.send(ctx, chatID, "Checking: "+pluralNames(torrents))
}

func (a *application) del(ctx context.Context, chatID int64, references []string) {
	torrents, err := a.selected(chatID, references, false)
	if err != nil {
		a.send(ctx, chatID, "del: "+err.Error())
		return
	}
	if err := a.rtorrent.Delete(false, torrents...); err != nil {
		a.logger.Printf("del: %s", err)
		a.send(ctx, chatID, mutationError("del", len(torrents), err))
		return
	}
	a.send(ctx, chatID, "Deleted: "+pluralNames(torrents))
}

func (a *application) deldata(ctx context.Context, chatID int64, arguments []string) {
	if len(arguments) != 2 || !strings.EqualFold(arguments[1], "confirm") {
		a.send(ctx, chatID, "deldata: use deldata HASH confirm")
		return
	}
	allTorrents, err := a.torrents(chatID)
	if err != nil {
		a.send(ctx, chatID, "deldata: "+err.Error())
		return
	}
	torrent, err := resolveTorrent(allTorrents, arguments[0])
	if err != nil {
		a.send(ctx, chatID, "deldata: "+err.Error())
		return
	}
	for _, other := range allTorrents {
		if other != torrent && pathsOverlap(torrent.Path, other.Path) {
			a.send(ctx, chatID, "deldata: torrent data overlaps another loaded torrent; metadata was not deleted")
			return
		}
	}
	root, relative, err := validateTorrentData(a.dataRoot, torrent.Path)
	if err != nil {
		a.send(ctx, chatID, "deldata: "+err.Error())
		return
	}
	defer root.Close()
	if err := deleteMetadata(a.rtorrent, torrent); err != nil {
		a.logger.Printf("deldata: %s", err)
		a.send(ctx, chatID, "deldata: "+err.Error())
		return
	}
	if err := root.RemoveAll(relative); err != nil {
		a.logger.Printf("deldata local removal: %s", err)
		a.send(ctx, chatID, fmt.Sprintf("Deleted torrent metadata, but could not remove local data for %s: %s", torrent.Name, err))
		return
	}
	a.send(ctx, chatID, "Deleted with data: "+torrent.Name)
}

func (a *application) sort(ctx context.Context, chatID int64, arguments []string) {
	if len(arguments) == 0 {
		a.send(ctx, chatID, "sort: [rev] name|downrate|uprate|size|ratio|age|upload")
		return
	}
	preference := sortPreference{}
	if strings.EqualFold(arguments[0], "rev") {
		preference.reverse = true
		arguments = arguments[1:]
	}
	if len(arguments) != 1 {
		a.send(ctx, chatID, "sort: [rev] name|downrate|uprate|size|ratio|age|upload")
		return
	}
	preference.key = strings.ToLower(arguments[0])
	switch preference.key {
	case "name", "downrate", "uprate", "size", "ratio", "age", "upload":
	default:
		a.send(ctx, chatID, "sort: unknown sorting method")
		return
	}
	a.sortMu.Lock()
	a.sorts[chatID] = preference
	a.sortMu.Unlock()
	direction := ""
	if preference.reverse {
		direction = "reversed "
	}
	a.send(ctx, chatID, "sort: by "+direction+preference.key)
}
