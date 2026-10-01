package main

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/pyed/rtapi"
)

func mutationError(action string, count int, err error) string {
	message := action + ": " + err.Error()
	if count > 1 {
		message += "; rTorrent may have applied the operation to some torrents, so refresh before retrying"
	}
	return message
}

func (a *application) add(ctx context.Context, chatID int64, sources []string) {
	if len(sources) == 0 {
		a.send(ctx, chatID, "add: needs at least one URL")
		return
	}
	// Confirming a link can take a while, so it happens off the update loop.
	// Links are handled in order so that a new torrent is not credited to the
	// wrong link.
	a.launch(ctx, func(addCtx context.Context) {
		for _, source := range sources {
			a.addSource(addCtx, chatID, source)
		}
	})
}

func (a *application) addSource(ctx context.Context, chatID int64, source string) {
	hash, name := magnetInfo(source)
	if name == "" {
		name = sourceName(source)
	}
	before, err := a.loadedHashes(ctx)
	if err != nil {
		a.send(ctx, chatID, "add: "+err.Error())
		return
	}
	if before[hash] {
		a.send(ctx, chatID, "add: "+name+" is already loaded")
		return
	}
	if err := a.rtorrent.DownloadContext(ctx, source); err != nil {
		a.logger.Printf("add: %s", err)
		a.send(ctx, chatID, "add: "+err.Error())
		return
	}
	a.confirmAdded(ctx, chatID, "add", name, hash, before)
}

func (a *application) loadedHashes(ctx context.Context) (map[string]bool, error) {
	torrents, err := a.rtorrent.TorrentsContext(ctx)
	if err != nil {
		return nil, err
	}
	hashes := make(map[string]bool, len(torrents))
	for _, torrent := range torrents {
		hashes[strings.ToUpper(torrent.Hash)] = true
	}
	return hashes, nil
}

// confirmAdded reports the torrent rTorrent loaded for a request, or that none
// appeared. rTorrent acknowledges links before it fetches or parses them, so
// the acknowledgement alone does not mean anything was added.
func (a *application) confirmAdded(ctx context.Context, chatID int64, label, name, hash string, before map[string]bool) {
	deadline := time.Now().Add(a.addTimeout)
	var lastErr error
	for {
		torrents, err := a.rtorrent.TorrentsContext(ctx)
		lastErr = err
		if err == nil {
			if added := findAdded(torrents, hash, before); added != nil {
				a.send(ctx, chatID, fmt.Sprintf("Added: <%s> %s", torrentRef(added, hashPrefixes(torrents)), added.Name))
				return
			}
		}
		if !time.Now().Before(deadline) || !waitFor(ctx, a.addPollInterval) {
			break
		}
	}
	switch {
	case ctx.Err() != nil:
	case lastErr != nil:
		a.send(ctx, chatID, label+": "+lastErr.Error())
	default:
		a.send(ctx, chatID, fmt.Sprintf("%s: rTorrent did not load %s within %s; the link or file may be invalid, or rTorrent may still be fetching it",
			label, name, a.addTimeout))
	}
}

// findAdded returns the torrent with hash or, when the hash is not known in
// advance, the newest torrent that was not loaded before.
func findAdded(torrents rtapi.Torrents, hash string, before map[string]bool) *rtapi.Torrent {
	var newest *rtapi.Torrent
	for _, torrent := range torrents {
		current := strings.ToUpper(torrent.Hash)
		if hash != "" {
			if current == hash {
				return torrent
			}
			continue
		}
		if !before[current] && (newest == nil || torrent.Age > newest.Age) {
			newest = torrent
		}
	}
	return newest
}

// magnetInfo returns the upper-case info-hash and display name of a magnet
// link. The hash is empty when source is not a magnet link with a BitTorrent v1
// info-hash, and the name is empty when source is not a magnet link at all.
func magnetInfo(source string) (hash, name string) {
	link, err := url.Parse(source)
	if err != nil || !strings.EqualFold(link.Scheme, "magnet") {
		return "", ""
	}
	query := link.Query()
	for _, topic := range query["xt"] {
		value, ok := strings.CutPrefix(strings.ToLower(topic), "urn:btih:")
		if !ok {
			continue
		}
		var raw []byte
		switch len(value) {
		case 40:
			raw, err = hex.DecodeString(value)
		case 32:
			raw, err = base32.StdEncoding.DecodeString(strings.ToUpper(value))
		default:
			continue
		}
		if err == nil && len(raw) == 20 {
			hash = strings.ToUpper(hex.EncodeToString(raw))
			break
		}
	}
	name = query.Get("dn")
	switch {
	case name != "":
	case hash != "":
		name = "magnet " + strings.ToLower(hash[:7])
	default:
		name = "magnet link"
	}
	return hash, name
}

// sourceName names a link in replies without echoing its query string, which
// often holds a tracker passkey.
func sourceName(source string) string {
	if link, err := url.Parse(source); err == nil && link.Scheme != "" {
		if base := path.Base(link.Path); base != "." && base != "/" {
			return base
		}
		if link.Host != "" {
			return link.Host
		}
	}
	return path.Base(source)
}

func (a *application) selected(ctx context.Context, chatID int64, references []string, allowAll bool) (rtapi.Torrents, error) {
	torrents, err := a.torrents(ctx, chatID)
	if err != nil {
		return nil, err
	}
	return selectTorrents(torrents, references, allowAll)
}

func (a *application) start(ctx context.Context, chatID int64, references []string) {
	torrents, err := a.selected(ctx, chatID, references, true)
	if err != nil {
		a.send(ctx, chatID, "start: "+err.Error())
		return
	}
	if err := a.rtorrent.StartContext(ctx, torrents...); err != nil {
		a.logger.Printf("start: %s", err)
		a.send(ctx, chatID, mutationError("start", len(torrents), err))
		return
	}
	a.send(ctx, chatID, "Started: "+pluralNames(torrents))
}

func (a *application) stop(ctx context.Context, chatID int64, references []string) {
	torrents, err := a.selected(ctx, chatID, references, true)
	if err != nil {
		a.send(ctx, chatID, "stop: "+err.Error())
		return
	}
	if err := a.rtorrent.StopContext(ctx, torrents...); err != nil {
		a.logger.Printf("stop: %s", err)
		a.send(ctx, chatID, mutationError("stop", len(torrents), err))
		return
	}
	a.send(ctx, chatID, "Stopped: "+pluralNames(torrents))
}

func (a *application) check(ctx context.Context, chatID int64, references []string) {
	torrents, err := a.selected(ctx, chatID, references, true)
	if err != nil {
		a.send(ctx, chatID, "check: "+err.Error())
		return
	}
	if err := a.rtorrent.CheckContext(ctx, torrents...); err != nil {
		a.logger.Printf("check: %s", err)
		a.send(ctx, chatID, mutationError("check", len(torrents), err))
		return
	}
	a.send(ctx, chatID, "Checking: "+pluralNames(torrents))
}

func (a *application) del(ctx context.Context, chatID int64, references []string) {
	torrents, err := a.selected(ctx, chatID, references, false)
	if err != nil {
		a.send(ctx, chatID, "del: "+err.Error())
		return
	}
	if err := a.rtorrent.DeleteMetadataContext(ctx, torrents...); err != nil {
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
	allTorrents, err := a.torrents(ctx, chatID)
	if err != nil {
		a.send(ctx, chatID, "deldata: "+err.Error())
		return
	}
	torrent, err := resolveTorrent(allTorrents, arguments[0])
	if err != nil {
		a.send(ctx, chatID, "deldata: "+err.Error())
		return
	}
	targetPath := dataPath(torrent)
	root, relative, err := validateTorrentData(a.dataRoot, targetPath)
	if err != nil {
		a.send(ctx, chatID, "deldata: "+err.Error())
		return
	}
	defer root.Close()
	if err := sharedDataConflict(torrent, targetPath, allTorrents); err != nil {
		a.send(ctx, chatID, "deldata: "+err.Error())
		return
	}
	if err := a.rtorrent.DeleteMetadataContext(ctx, torrent); err != nil {
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
