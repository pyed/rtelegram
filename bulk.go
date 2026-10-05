package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

// Bulk actions act on every torrent a list shows: start, stop, check, label,
// remove, or remove with data. Each asks first, saying how many torrents it
// acts on, and then acts only on those of the torrents the list showed when
// its menu opened that the list still shows, so it never touches a torrent
// the user did not see.

const (
	bulkMenu    = ""      // the menu of actions
	bulkPick    = "pick"  // choosing a label
	bulkStart   = "start" // the rest are actions awaiting confirmation
	bulkStop    = "stop"
	bulkCheck   = "check"
	bulkLabel   = "label"
	bulkDel     = "del"
	bulkDelData = "deldata"
	bulkDone    = "done" // the outcome, with a way back to the list
	// maxBulkNames is how many torrents a confirmation or outcome names.
	maxBulkNames = 10
)

// A bulkAction is a list's menu of actions on all its torrents, a label
// picker for them, or the action it asks to confirm.
type bulkAction struct {
	spec   listSpec // the list
	page   int      // the page to go back to
	hashes []string // the torrents the list showed when the menu opened
	action string   // what the screen shows: bulkMenu, bulkPick, an action, or bulkDone
	label  string   // the label to set, for bulkLabel; "" removes labels
}

// describe names count torrents of a list, as "25 torrents with errors".
func (spec listSpec) describe(count int) string {
	torrents := plural(count, "torrent")
	switch spec.kind {
	case "list":
		if spec.query == "" {
			return "all " + torrents
		}
		return fmt.Sprintf("%s whose tracker matches %q", torrents, spec.query)
	case "down":
		return torrents + " downloading"
	case "seeding":
		return torrents + " seeding"
	case "checking":
		return torrents + " being checked"
	case "paused":
		return torrents + " stopped"
	case "errors":
		return torrents + " with errors"
	case "search":
		return fmt.Sprintf("%s whose name contains %q", torrents, spec.query)
	case "latest":
		return "the " + torrents + " added last"
	case "label":
		if spec.query == "" {
			return torrents + " without a label"
		}
		return fmt.Sprintf("%s labelled %s", torrents, spec.query)
	case "unregistered":
		return fmt.Sprintf("%s their tracker no longer has", torrents)
	}
	return torrents
}

// plural counts things: "1 torrent", "2 torrents".
func plural(count int, thing string) string {
	if count == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", count, thing)
}

// bulkAllows reports whether a list offers an action. Removing is not
// offered for the list of every torrent: one tap must never remove the
// whole library.
func (a *application) bulkAllows(spec listSpec, action string) bool {
	switch action {
	case bulkStart, bulkStop, bulkCheck, bulkLabel, bulkPick:
		return true
	case bulkDel:
		return spec.kind != "list" || spec.query != ""
	case bulkDelData:
		return a.dataRoot != "" && (spec.kind != "list" || spec.query != "")
	}
	return false
}

// bulkTargets returns the torrents of current whose hashes are in hashes,
// in the order of current.
func bulkTargets(current rtapi.Torrents, hashes []string) rtapi.Torrents {
	wanted := make(map[string]bool, len(hashes))
	for _, hash := range hashes {
		wanted[strings.ToUpper(hash)] = true
	}
	return filterTorrents(current, func(torrent *rtapi.Torrent) bool { return wanted[strings.ToUpper(torrent.Hash)] })
}

// namesOf lists torrents' names a line each, up to maxBulkNames.
func namesOf(torrents rtapi.Torrents) string {
	var text strings.Builder
	for i, torrent := range torrents {
		if i == maxBulkNames {
			fmt.Fprintf(&text, "\n• and %d more", len(torrents)-i)
			break
		}
		text.WriteString("\n• " + torrent.Name)
	}
	return text.String()
}

// pressBulk opens a list's menu of bulk actions, or, given an action, asks
// to confirm it straight away. It notes which torrents the list shows now.
func (a *application) pressBulk(ctx context.Context, key screenKey, scr *screen, action string) (string, bool) {
	if scr.list == nil || action != bulkMenu && !a.bulkAllows(*scr.list, action) {
		return "This button no longer applies.", true
	}
	torrents, _, err := a.listTorrents(ctx, key.chatID, *scr.list)
	if err != nil {
		return err.Error(), true
	}
	hashes := make([]string, len(torrents))
	for i, torrent := range torrents {
		hashes[i] = torrent.Hash
	}
	return a.redrawBulk(ctx, key, &screen{bulk: &bulkAction{spec: *scr.list, page: scr.page, hashes: hashes, action: action}}, "")
}

// pressBulkAction chooses an action from a bulk menu: a label picker for
// labelling, and a confirmation for the rest.
func (a *application) pressBulkAction(ctx context.Context, key screenKey, scr *screen, action string) (string, bool) {
	if scr.bulk == nil || scr.bulk.action != bulkMenu || action == bulkLabel || !a.bulkAllows(scr.bulk.spec, action) {
		return "This button no longer applies.", true
	}
	next, bulk := *scr, *scr.bulk
	bulk.action = action
	next.bulk = &bulk
	return a.redrawBulk(ctx, key, &next, "")
}

// redrawBulk shows a bulk screen in an existing message: the menu, a label
// picker, or a confirmation, for those of the torrents noted that the list
// still shows.
func (a *application) redrawBulk(ctx context.Context, key screenKey, scr *screen, toast string) (string, bool) {
	bulk := scr.bulk
	current, _, err := a.listTorrents(ctx, key.chatID, bulk.spec)
	if err != nil {
		return err.Error(), true
	}
	targets := bulkTargets(current, bulk.hashes)
	if len(targets) == 0 {
		return a.redrawList(ctx, key, bulk.spec, bulk.page, "None of those torrents is in the list any more.")
	}
	next := *scr
	var text string
	var keyboard *models.InlineKeyboardMarkup
	if bulk.action == bulkPick {
		all, err := a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
		if err != nil {
			return err.Error(), true
		}
		next.picks = labelChoices(all)
		text = fmt.Sprintf("Label %s: choose a label, or reply to this message with a new one.%s", bulk.spec.describe(len(targets)), namesOf(targets))
		rows := append(renderLabelPicker(next.picks, true), []models.InlineKeyboardButton{button("« Back", "back")})
		keyboard = &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	} else {
		text, keyboard = a.renderBulk(bulk, targets)
	}
	if err := a.editScreen(ctx, key, text, keyboard); err != nil {
		return err.Error(), true
	}
	a.screens.put(key, &next)
	return toast, false
}

// renderBulk renders a bulk menu or confirmation for targets.
func (a *application) renderBulk(bulk *bulkAction, targets rtapi.Torrents) (string, *models.InlineKeyboardMarkup) {
	count := len(targets)
	back := []models.InlineKeyboardButton{button("« Back", "back")}
	if bulk.action == bulkMenu {
		text := fmt.Sprintf("What should happen to %s?%s", bulk.spec.describe(count), namesOf(targets))
		rows := [][]models.InlineKeyboardButton{
			{button("▶ Start", "ba:"+bulkStart), button("⏸ Stop", "ba:"+bulkStop), button("🔍 Check", "ba:"+bulkCheck)},
			{button("🏷 Label", "ba:"+bulkPick)},
		}
		var removals []models.InlineKeyboardButton
		if a.bulkAllows(bulk.spec, bulkDel) {
			removals = append(removals, button("🗑 Remove", "ba:"+bulkDel))
		}
		if a.bulkAllows(bulk.spec, bulkDelData) {
			removals = append(removals, button("💣 Remove + data", "ba:"+bulkDelData))
		}
		if len(removals) > 0 {
			rows = append(rows, removals)
		}
		return text, &models.InlineKeyboardMarkup{InlineKeyboard: append(rows, back)}
	}
	described := bulk.spec.describe(count)
	var question, confirm string
	switch bulk.action {
	case bulkStart:
		question, confirm = "Start "+described+"?", fmt.Sprintf("▶ Start %d", count)
	case bulkStop:
		question, confirm = "Stop "+described+"?", fmt.Sprintf("⏸ Stop %d", count)
	case bulkCheck:
		question, confirm = "Verify the data of "+described+"?", fmt.Sprintf("🔍 Check %d", count)
	case bulkLabel:
		if bulk.label == "" {
			question, confirm = "Remove the label of "+described+"?", fmt.Sprintf("✖ Unlabel %d", count)
		} else {
			question, confirm = fmt.Sprintf("Label %s: %s?", described, bulk.label), fmt.Sprintf("🏷 Label %d", count)
		}
	case bulkDel:
		question, confirm = "Remove "+described+" from rTorrent? Their data stays on disk.", fmt.Sprintf("🗑 Remove %d", count)
	case bulkDelData:
		var size uint64
		for _, torrent := range targets {
			size += torrent.Completed
		}
		question = fmt.Sprintf("Remove %s and delete their data, %s, from disk? This cannot be undone.", described, formatBytes(size))
		confirm = fmt.Sprintf("💣 Delete %d + data", count)
	}
	return question + namesOf(targets), &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{button(confirm, "bc"), button("Cancel", "back")},
	}}
}

// confirmBulk does a confirmed bulk action in the background, since removing
// data can take minutes, and then shows the outcome in place of the
// confirmation. The confirmation goes first, so a second tap does nothing.
func (a *application) confirmBulk(ctx context.Context, key screenKey, scr *screen) (string, bool) {
	bulk := scr.bulk
	if bulk == nil || bulk.action == bulkMenu || bulk.action == bulkPick || bulk.action == bulkDone || !a.bulkAllows(bulk.spec, bulk.action) {
		return "This button no longer applies.", true
	}
	a.screens.forget(key)
	working := "Working on it…"
	if err := a.editScreen(ctx, key, working, nil); err != nil {
		return err.Error(), true
	}
	a.launch(ctx, func(bulkCtx context.Context) {
		a.bulkMu.Lock()
		defer a.bulkMu.Unlock()
		outcome := a.runBulk(bulkCtx, key.chatID, bulk)
		back := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{button("« Back to the list", "back")}}}
		if err := a.editScreen(bulkCtx, key, outcome, back); err != nil {
			a.send(bulkCtx, key.chatID, outcome)
			return
		}
		a.screens.put(key, &screen{bulk: &bulkAction{spec: bulk.spec, page: bulk.page, action: bulkDone}})
	})
	return working, false
}

// runBulk does a bulk action on those of the torrents noted that the list
// still shows, and says what happened.
func (a *application) runBulk(ctx context.Context, chatID int64, bulk *bulkAction) string {
	current, _, err := a.listTorrents(ctx, chatID, bulk.spec)
	if err != nil {
		return "Nothing was done: " + err.Error()
	}
	targets := bulkTargets(current, bulk.hashes)
	if len(targets) == 0 {
		return "Nothing was done: none of those torrents is in the list any more."
	}
	var note string
	if left := len(bulk.hashes) - len(targets); left > 0 {
		note = fmt.Sprintf("\n\nNot touched: %s that left the list meanwhile.", plural(left, "torrent"))
	}
	count := plural(len(targets), "torrent")
	var done string
	switch bulk.action {
	case bulkStart:
		err, done = a.rtorrent.StartContext(ctx, targets...), "Started "+count+"."
	case bulkStop:
		err, done = a.rtorrent.StopContext(ctx, targets...), "Stopped "+count+"."
	case bulkCheck:
		err, done = a.rtorrent.CheckContext(ctx, targets...), "Checking "+count+"."
	case bulkLabel:
		err, done = a.rtorrent.SetLabelContext(ctx, encodeLabel(bulk.label), targets...), fmt.Sprintf("Labelled %s: %s.", count, bulk.label)
		if bulk.label == "" {
			done = "Removed the label of " + count + "."
		}
	case bulkDel:
		err, done = a.rtorrent.DeleteMetadataContext(ctx, targets...), "Removed "+count+" from rTorrent; their data stays on disk."
	case bulkDelData:
		hashes := make([]string, len(targets))
		for i, torrent := range targets {
			hashes[i] = torrent.Hash
		}
		return a.removeWithData(ctx, hashes) + note
	default:
		return "Nothing was done."
	}
	if err != nil {
		a.logger.Printf("[ERROR] %s %s: %s", bulk.action, count, err)
		return mutationError(bulk.action, len(targets), err) + note
	}
	return done + namesOf(targets) + note
}

// removeWithData removes torrents, named by hash, from rTorrent and deletes
// their data, with deldata's safeguards: data is deleted only inside
// -data-root, never through a symbolic link, and never while another torrent
// may use it. A torrent whose data overlaps that of a torrent that stays
// loaded is kept, with its data, and nothing is removed while rTorrent has
// not said where some torrent's data is. Torrents are erased first, and data
// is deleted only once rTorrent has let go of every torrent that used it. It
// says what it did.
func (a *application) removeWithData(ctx context.Context, hashes []string) string {
	if a.dataRoot == "" {
		return "Nothing was done: deleting data is off; set -data-root to the directory on this machine where rTorrent keeps data."
	}
	all, err := a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
	if err != nil {
		return "Nothing was done: " + err.Error()
	}
	paths, unknown := canonicalPaths(all)
	if unknown != nil {
		return fmt.Sprintf("Nothing was done: rTorrent did not report where %s keeps its data, so it may share these torrents' data.", unknown.Name)
	}
	targets := bulkTargets(all, hashes)
	kept := make(map[*rtapi.Torrent]string) // the targets that stay loaded, and why
	going := make(map[*rtapi.Torrent]bool)
	for _, torrent := range targets {
		if err := a.checkDeletable(torrent); err != nil {
			kept[torrent] = err.Error()
			continue
		}
		going[torrent] = true
	}
	// A torrent goes only when every torrent that shares its data goes too.
	overlaps := make(map[*rtapi.Torrent]rtapi.Torrents, len(going))
	for torrent := range going {
		for _, other := range all {
			if other != torrent && overlapCanonical(paths[torrent], paths[other]) {
				overlaps[torrent] = append(overlaps[torrent], other)
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, torrent := range targets {
			if !going[torrent] {
				continue
			}
			if stays := slices.IndexFunc(overlaps[torrent], func(other *rtapi.Torrent) bool { return !going[other] }); stays >= 0 {
				delete(going, torrent)
				kept[torrent] = "its data overlaps that of " + overlaps[torrent][stays].Name + ", which stays"
				changed = true
			}
		}
	}

	leaving := filterTorrents(targets, func(torrent *rtapi.Torrent) bool { return going[torrent] })
	var erased rtapi.Torrents
	dataKept := make(map[*rtapi.Torrent]string) // the erased torrents whose data stays, and why
	if len(leaving) > 0 {
		eraseErr := a.rtorrent.DeleteMetadataContext(ctx, leaving...)
		fresh, err := a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
		if err != nil {
			// Which torrents went is unknown, so no data is deleted.
			a.logger.Printf("[ERROR] listing torrents after removing %d: %s", len(leaving), err)
			return fmt.Sprintf("rTorrent was asked to remove %s, but they could not be listed afterwards (%s), so no data was deleted. Check the list before trying again.",
				plural(len(leaving), "torrent"), err) + keptReport(targets, kept)
		}
		erased, dataKept = a.deleteErasedData(leaving, paths, fresh)
		for _, torrent := range leaving {
			if slices.Contains(erased, torrent) {
				continue
			}
			kept[torrent] = "rTorrent did not remove it"
			if eraseErr != nil {
				kept[torrent] += ": " + eraseErr.Error()
			}
		}
		if eraseErr != nil {
			a.logger.Printf("[ERROR] removing %d torrents with their data: %s", len(leaving), eraseErr)
		}
	}

	var report strings.Builder
	var deleted rtapi.Torrents
	var size uint64
	for _, torrent := range erased {
		if _, ok := dataKept[torrent]; !ok {
			deleted = append(deleted, torrent)
			size += torrent.Completed
		}
	}
	switch {
	case len(deleted) > 0:
		fmt.Fprintf(&report, "Removed %s and deleted their data, %s.%s", plural(len(deleted), "torrent"), formatBytes(size), namesOf(deleted))
	case len(erased) == 0:
		report.WriteString("Nothing was removed.")
	}
	if len(dataKept) > 0 {
		fmt.Fprintf(&report, "\n\nRemoved %s from rTorrent, but kept their data:%s", plural(len(dataKept), "torrent"), reasons(erased, dataKept))
	}
	report.WriteString(keptReport(targets, kept))
	return strings.TrimLeft(report.String(), "\n")
}

// keptReport says which targets stayed loaded, with their data, and why.
func keptReport(targets rtapi.Torrents, kept map[*rtapi.Torrent]string) string {
	if len(kept) == 0 {
		return ""
	}
	return fmt.Sprintf("\n\nKept %s loaded, with their data:%s", plural(len(kept), "torrent"), reasons(targets, kept))
}

// deleteErasedData deletes the data of those of leaving that are not in
// fresh, the torrents rTorrent has once it erased leaving, and returns them,
// with why it kept the data of any: data that a torrent still loaded
// overlaps is kept.
func (a *application) deleteErasedData(leaving rtapi.Torrents, paths map[*rtapi.Torrent]string, fresh rtapi.Torrents) (rtapi.Torrents, map[*rtapi.Torrent]string) {
	dataKept := make(map[*rtapi.Torrent]string)
	loaded := make(map[string]bool, len(fresh))
	for _, torrent := range fresh {
		loaded[strings.ToUpper(torrent.Hash)] = true
	}
	erased := filterTorrents(leaving, func(torrent *rtapi.Torrent) bool { return !loaded[strings.ToUpper(torrent.Hash)] })
	freshPaths, unknown := canonicalPaths(fresh)
	var removed []string
	for _, torrent := range erased {
		path := paths[torrent]
		if unknown != nil {
			dataKept[torrent] = "rTorrent did not report where " + unknown.Name + " keeps its data, so it may share it"
			continue
		}
		if user := slices.IndexFunc(fresh, func(other *rtapi.Torrent) bool { return overlapCanonical(path, freshPaths[other]) }); user >= 0 {
			dataKept[torrent] = "its data overlaps that of " + fresh[user].Name + ", which is loaded"
			continue
		}
		// Data inside data already deleted is gone too.
		if slices.ContainsFunc(removed, func(parent string) bool { return containsPath(parent, path) }) {
			continue
		}
		root, relative, err := validateTorrentData(a.dataRoot, dataPath(torrent))
		if err != nil {
			dataKept[torrent] = err.Error()
			continue
		}
		err = root.RemoveAll(relative)
		root.Close()
		if err != nil {
			a.logger.Printf("[ERROR] deleting the data of %s: %s", torrent.Name, err)
			dataKept[torrent] = "deleting it failed: " + err.Error()
			continue
		}
		removed = append(removed, path)
	}
	return erased, dataKept
}

// reasons lists, in the order of torrents, each torrent in why with the
// reason, up to maxBulkNames.
func reasons(torrents rtapi.Torrents, why map[*rtapi.Torrent]string) string {
	var text strings.Builder
	listed := 0
	for _, torrent := range torrents {
		reason, ok := why[torrent]
		if !ok {
			continue
		}
		if listed == maxBulkNames {
			fmt.Fprintf(&text, "\n• and %d more", len(why)-listed)
			break
		}
		fmt.Fprintf(&text, "\n• %s: %s", torrent.Name, reason)
		listed++
	}
	return text.String()
}

// checkDeletable says why a torrent's data cannot be deleted, if it cannot:
// it must be inside -data-root, exist, and not be a symbolic link.
func (a *application) checkDeletable(torrent *rtapi.Torrent) error {
	root, _, err := validateTorrentData(a.dataRoot, dataPath(torrent))
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("it has no data where rTorrent says, so remove it without its data instead")
	}
	if err != nil {
		return err
	}
	return root.Close()
}

// canonicalPaths returns where each torrent keeps its data, with symbolic
// links resolved, and a torrent rTorrent has not said that of, if any.
func canonicalPaths(torrents rtapi.Torrents) (map[*rtapi.Torrent]string, *rtapi.Torrent) {
	paths := make(map[*rtapi.Torrent]string, len(torrents))
	var unknown *rtapi.Torrent
	for _, torrent := range torrents {
		path := dataPath(torrent)
		if !filepath.IsAbs(path) {
			unknown = cmp.Or(unknown, torrent)
			continue
		}
		paths[torrent] = canonicalPath(path)
	}
	return paths, unknown
}

// overlapCanonical reports whether two canonical paths are the same, or one
// is inside the other.
func overlapCanonical(left, right string) bool {
	return left != "" && right != "" && (containsPath(left, right) || containsPath(right, left))
}

// containsPath reports whether path is dir or inside it.
func containsPath(dir, path string) bool {
	relative, err := filepath.Rel(dir, path)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}
