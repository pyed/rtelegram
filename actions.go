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
	a.addLink(ctx, chatID, "add", source, "", rtapi.DotTorrentWithOptions{})
}

// addLink loads a URL or magnet link into rTorrent and reports, prefixed with
// label, whether the torrent appeared. name, when given, is used in replies
// instead of a name taken from the link.
func (a *application) addLink(ctx context.Context, chatID int64, label, source, name string, options rtapi.DotTorrentWithOptions) {
	hash, magnetName := magnetInfo(source)
	if name == "" {
		name = magnetName
	}
	if name == "" {
		name = sourceName(source)
	}
	before, err := a.loadedHashes(ctx)
	if err != nil {
		a.send(ctx, chatID, label+": "+err.Error())
		return
	}
	if before[hash] {
		a.send(ctx, chatID, label+": "+name+" is already loaded")
		return
	}
	options.Link, options.Stopped = source, a.addStopped
	if err := a.rtorrent.DownloadWithOptionsContext(ctx, &options); err != nil {
		a.logger.Printf("%s: %s", label, err)
		a.send(ctx, chatID, label+": "+err.Error())
		return
	}
	a.confirmAdded(ctx, chatID, label, name, hash, before)
}

func (a *application) loadedHashes(ctx context.Context) (map[string]bool, error) {
	hashes, err := a.rtorrent.HashesContext(ctx)
	if err != nil {
		return nil, err
	}
	loaded := make(map[string]bool, len(hashes))
	for _, hash := range hashes {
		loaded[strings.ToUpper(hash)] = true
	}
	return loaded, nil
}

// confirmAdded reports the torrent rTorrent loaded for a request, or that none
// appeared. rTorrent acknowledges links before it fetches or parses them, so
// the acknowledgement alone does not mean anything was added.
func (a *application) confirmAdded(ctx context.Context, chatID int64, label, name, hash string, before map[string]bool) {
	deadline := time.Now().Add(a.addTimeout)
	var lastErr error
	for {
		added, hashes, err := a.findAdded(ctx, hash, before)
		lastErr = err
		if added != nil {
			a.send(ctx, chatID, fmt.Sprintf("Added: <%s> %s", torrentRef(added, prefixesOf(hashes)), added.Name))
			return
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
// advance, the newest torrent that was not loaded before; nil if there is none
// yet. It also returns the hashes of every loaded torrent. Listing hashes and
// then fetching only the new torrent keeps polling cheap in large libraries.
func (a *application) findAdded(ctx context.Context, hash string, before map[string]bool) (*rtapi.Torrent, []string, error) {
	hashes, err := a.rtorrent.HashesContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	var newest *rtapi.Torrent
	for _, current := range hashes {
		upper := strings.ToUpper(current)
		if (hash != "" && upper != hash) || (hash == "" && before[upper]) {
			continue
		}
		torrent, err := a.rtorrent.GetTorrentContext(ctx, current)
		if err != nil {
			return nil, nil, err
		}
		if hash != "" {
			return torrent, hashes, nil
		}
		if newest == nil || torrent.Age > newest.Age {
			newest = torrent
		}
	}
	return newest, hashes, nil
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

// deldata deletes a torrent and its data. "deldata HASH" asks with buttons,
// and "deldata HASH confirm" deletes straight away.
func (a *application) deldata(ctx context.Context, chatID int64, arguments []string) {
	switch {
	case len(arguments) == 2 && strings.EqualFold(arguments[1], "confirm"):
		message, _ := a.deleteWithData(ctx, chatID, arguments[0])
		a.send(ctx, chatID, message)
	case len(arguments) == 1:
		if a.dataRoot == "" {
			a.send(ctx, chatID, "deldata: deldata is disabled; configure an absolute -data-root")
			return
		}
		torrents, err := a.selected(ctx, chatID, arguments, false)
		if err == nil {
			err = a.rtorrent.TrackersContext(ctx, torrents)
		}
		if err != nil {
			a.send(ctx, chatID, "deldata: "+err.Error())
			return
		}
		a.sendCard(ctx, chatID, torrents[0], "deldata")
	default:
		a.send(ctx, chatID, "deldata: use deldata HASH, or deldata HASH confirm")
	}
}

// deleteWithData erases a torrent's metadata and then deletes its data. It
// returns what happened, and whether the metadata was erased.
func (a *application) deleteWithData(ctx context.Context, chatID int64, reference string) (string, bool) {
	allTorrents, err := a.torrents(ctx, chatID)
	if err != nil {
		return "deldata: " + err.Error(), false
	}
	torrent, err := resolveTorrent(allTorrents, reference)
	if err != nil {
		return "deldata: " + err.Error(), false
	}
	targetPath := dataPath(torrent)
	root, relative, err := validateTorrentData(a.dataRoot, targetPath)
	if err != nil {
		return "deldata: " + err.Error(), false
	}
	defer root.Close()
	if err := sharedDataConflict(torrent, targetPath, allTorrents); err != nil {
		return "deldata: " + err.Error(), false
	}
	if err := a.rtorrent.DeleteMetadataContext(ctx, torrent); err != nil {
		a.logger.Printf("deldata: %s", err)
		return "deldata: " + err.Error(), false
	}
	if err := root.RemoveAll(relative); err != nil {
		a.logger.Printf("deldata local removal: %s", err)
		return fmt.Sprintf("Deleted torrent metadata, but could not remove local data for %s: %s", torrent.Name, err), true
	}
	return "Deleted with data: " + torrent.Name, true
}

func (a *application) sort(ctx context.Context, chatID int64, arguments []string) {
	if len(arguments) == 0 {
		a.send(ctx, chatID, "sort: [rev] name|downrate|uprate|size|ratio|age|upload")
		return
	}
	reverse := strings.EqualFold(arguments[0], "rev")
	if reverse {
		arguments = arguments[1:]
	}
	if len(arguments) != 1 {
		a.send(ctx, chatID, "sort: [rev] name|downrate|uprate|size|ratio|age|upload")
		return
	}
	key := strings.ToLower(arguments[0])
	orders, ok := sortings[key]
	if !ok {
		a.send(ctx, chatID, "sort: unknown sorting method")
		return
	}
	sorting, direction := orders[0], ""
	if reverse {
		sorting, direction = orders[1], "reversed "
	}
	reply := "sort: by " + direction + key
	err := a.state.update(func(data *stateData) {
		if data.Sorts == nil {
			data.Sorts = make(map[int64]rtapi.Sorting)
		}
		data.Sorts[chatID] = sorting
	})
	if err != nil {
		a.logger.Printf("[ERROR] sort: %s", err)
		reply += " (not saved, so a restart will forget it: " + err.Error() + ")"
	}
	a.send(ctx, chatID, reply)
}

// sortings maps the sort command's keys to rtapi's ascending and descending
// orders.
var sortings = map[string][2]rtapi.Sorting{
	"name":     {rtapi.ByName, rtapi.ByNameRev},
	"downrate": {rtapi.ByDownRate, rtapi.ByDownRateRev},
	"uprate":   {rtapi.ByUpRate, rtapi.ByUpRateRev},
	"size":     {rtapi.BySize, rtapi.BySizeRev},
	"ratio":    {rtapi.ByRatio, rtapi.ByRatioRev},
	"age":      {rtapi.ByAge, rtapi.ByAgeRev},
	"upload":   {rtapi.ByUpTotal, rtapi.ByUpTotalRev},
}
