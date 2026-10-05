package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

// Labels are ruTorrent's: rTorrent keeps a torrent's label in d.custom1,
// where ruTorrent stores it percent-encoded, as JavaScript's
// encodeURIComponent writes it, and decodes it to show it. rtelegram reads
// and writes labels the same way, so both show the same labels.

const (
	// maxLabelChoices is how many labels a picker offers, the most used first.
	maxLabelChoices = 20
	// maxLabelButtons is how many labels /labels has buttons for.
	maxLabelButtons = 50
	// maxLabelRunes bounds a label typed in a chat.
	maxLabelRunes = 100
	// maxBreakdownLabels is how many labels a digest names.
	maxBreakdownLabels = 8
	// noLabel stands for "no label" in commands.
	noLabel = "-"
)

// encodeLabel encodes a label as ruTorrent stores it.
func encodeLabel(label string) string {
	var encoded strings.Builder
	for i := 0; i < len(label); i++ {
		c := label[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			encoded.WriteByte(c)
		} else {
			fmt.Fprintf(&encoded, "%%%02X", c)
		}
	}
	return encoded.String()
}

// labelOf returns a torrent's label as ruTorrent shows it, or "" when it has
// none. A label that is not percent-encoded, as other programs may set it,
// reads as it is.
func labelOf(torrent *rtapi.Torrent) string {
	label, err := url.PathUnescape(torrent.Label)
	if err != nil || !utf8.ValidString(label) {
		label = strings.ToValidUTF8(torrent.Label, "�")
	}
	return strings.TrimSpace(label)
}

// checkLabel cleans up a label typed in a chat, or says why it cannot be one.
func checkLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	switch {
	case label == "":
		return "", errors.New("the label is empty")
	case strings.ContainsAny(label, "\r\n"):
		return "", errors.New("a label is one line")
	case utf8.RuneCountInString(label) > maxLabelRunes:
		return "", fmt.Errorf("a label has at most %d characters", maxLabelRunes)
	}
	return label, nil
}

// labelCounts counts the torrents with each label; "" counts those without.
func labelCounts(torrents rtapi.Torrents) map[string]int {
	counts := make(map[string]int)
	for _, torrent := range torrents {
		counts[labelOf(torrent)]++
	}
	return counts
}

// labelChoices returns the labels a picker offers: those torrents have, the
// most used first.
func labelChoices(torrents rtapi.Torrents) []string {
	counts := labelCounts(torrents)
	delete(counts, "")
	labels := slices.SortedFunc(maps.Keys(counts), func(x, y string) int {
		return cmp.Or(cmp.Compare(counts[y], counts[x]), cmp.Compare(x, y))
	})
	return labels[:min(len(labels), maxLabelChoices)]
}

// labelBreakdown says how many of torrents have each label, the most used
// first, as " (Movies 3, TV 2, no label 1)", or "" when none has a label.
func labelBreakdown(torrents rtapi.Torrents) string {
	counts := labelCounts(torrents)
	if counts[""] == len(torrents) {
		return ""
	}
	labels := slices.SortedFunc(maps.Keys(counts), func(x, y string) int {
		if (x == "") != (y == "") {
			return unlabelledLast(x, y)
		}
		return cmp.Or(cmp.Compare(counts[y], counts[x]), cmp.Compare(x, y))
	})
	var parts []string
	for i, label := range labels {
		if i == maxBreakdownLabels {
			parts = append(parts, fmt.Sprintf("%d more labels", len(labels)-i))
			break
		}
		name := label
		if label == "" {
			name = "no label"
		}
		parts = append(parts, fmt.Sprintf("%s %d", name, counts[label]))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// unlabelledLast orders labels as ruTorrent lists them, ignoring case, with
// "", no label, last.
func unlabelledLast(x, y string) int {
	if (x == "") != (y == "") {
		if x == "" {
			return 1
		}
		return -1
	}
	return cmp.Or(cmp.Compare(strings.ToLower(x), strings.ToLower(y)), cmp.Compare(x, y))
}

// labels answers /labels with how many torrents have each label, and a
// button for each that lists them; /labels NAME lists them directly, and
// /labels - the torrents without a label.
func (a *application) labels(ctx context.Context, chatID int64, arguments []string) {
	if len(arguments) > 0 {
		label := strings.Join(arguments, " ")
		if label == noLabel {
			label = ""
		}
		a.showList(ctx, chatID, listSpec{kind: "label", query: label})
		return
	}
	torrents, err := a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
	if err != nil {
		a.send(ctx, chatID, "labels: "+err.Error())
		return
	}
	counts := labelCounts(torrents)
	if counts[""] == len(torrents) {
		a.send(ctx, chatID, "No torrent has a label. Set one with /setlabel, or with l=LABEL in a .torrent file's caption.")
		return
	}
	labels := slices.SortedFunc(maps.Keys(counts), unlabelledLast)
	var text strings.Builder
	text.WriteString("Labels")
	var rows [][]models.InlineKeyboardButton
	var row []models.InlineKeyboardButton
	for i, label := range labels {
		name := label
		if label == "" {
			name = "No label"
		}
		fmt.Fprintf(&text, "\n%s: %d", name, counts[label])
		if i == maxLabelButtons {
			continue
		}
		row = append(row, button(fmt.Sprintf("%s (%d)", buttonName(name), counts[label]), "lo:"+strconv.Itoa(i)))
		if len(row) == 2 {
			rows, row = append(rows, row), nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	text.WriteString("\n\nTap a label to list its torrents.")
	a.sendScreen(ctx, chatID, text.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}, &screen{labels: labels[:min(len(labels), maxLabelButtons)]})
}

// pressLabels lists the torrents with the label a /labels button names.
func (a *application) pressLabels(ctx context.Context, key screenKey, scr *screen, choice string) (string, bool) {
	index, err := strconv.Atoi(choice)
	if err != nil || index < 0 || index >= len(scr.labels) {
		return "This button no longer applies.", true
	}
	return a.redrawList(ctx, key, listSpec{kind: "label", query: scr.labels[index]}, 0, "")
}

// setLabel answers /setlabel HASH LABEL by labelling a torrent, /setlabel
// HASH - by removing its label, and /setlabel HASH with a picker.
func (a *application) setLabel(ctx context.Context, chatID int64, arguments []string) {
	if len(arguments) == 0 {
		a.send(ctx, chatID, "setlabel: use setlabel HASH LABEL, or setlabel HASH - to remove the label")
		return
	}
	torrents, err := a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
	if err != nil {
		a.send(ctx, chatID, "setlabel: "+err.Error())
		return
	}
	torrent, err := resolveTorrent(torrents, arguments[0])
	if err != nil {
		a.send(ctx, chatID, "setlabel: "+err.Error())
		return
	}
	if len(arguments) == 1 {
		scr := &screen{hash: torrent.Hash, confirm: "label", picks: labelChoices(torrents)}
		text, keyboard := a.renderCard(torrent, scr)
		a.sendScreen(ctx, chatID, text, keyboard, scr)
		return
	}
	label := strings.Join(arguments[1:], " ")
	if label != noLabel {
		if label, err = checkLabel(label); err != nil {
			a.send(ctx, chatID, "setlabel: "+err.Error())
			return
		}
	}
	done, err := a.applyLabel(ctx, torrent, label)
	if err != nil {
		done = "setlabel: " + err.Error()
	}
	a.send(ctx, chatID, done)
}

// applyLabel sets a torrent's label, or removes it for "" or noLabel, and
// says what it did.
func (a *application) applyLabel(ctx context.Context, torrent *rtapi.Torrent, label string) (string, error) {
	if label == noLabel {
		label = ""
	}
	if err := a.rtorrent.SetLabelContext(ctx, encodeLabel(label), torrent); err != nil {
		return "", err
	}
	if label == "" {
		return "Removed the label of " + torrent.Name, nil
	}
	return fmt.Sprintf("Labelled %s: %s", torrent.Name, label), nil
}

// renderLabelPicker renders the buttons that choose a label from picks, and
// one that removes the label when removable.
func renderLabelPicker(picks []string, removable bool) [][]models.InlineKeyboardButton {
	var rows [][]models.InlineKeyboardButton
	var row []models.InlineKeyboardButton
	for i, label := range picks {
		row = append(row, button("🏷 "+buttonName(label), "lb:"+strconv.Itoa(i)))
		if len(row) == 2 {
			rows, row = append(rows, row), nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	if removable {
		rows = append(rows, []models.InlineKeyboardButton{button("✖ No label", "lb:"+noLabel)})
	}
	return rows
}

// pickedLabel returns the label a picker button names, "" for none.
func pickedLabel(picks []string, choice string) (string, bool) {
	if choice == noLabel {
		return "", true
	}
	index, err := strconv.Atoi(choice)
	if err != nil || index < 0 || index >= len(picks) {
		return "", false
	}
	return picks[index], true
}

// picking reports whether a screen is choosing a label.
func (scr *screen) picking() bool {
	return scr.hash != "" && scr.confirm == "label" || scr.bulk != nil && scr.bulk.action == bulkPick
}

// pressLabel applies the label a picker button chooses: to the torrent of a
// card, or, once confirmed, to a list's torrents.
func (a *application) pressLabel(ctx context.Context, key screenKey, scr *screen, choice string) (string, bool) {
	if !scr.picking() {
		return "This button no longer applies.", true
	}
	label, ok := pickedLabel(scr.picks, choice)
	if !ok {
		return "This button no longer applies.", true
	}
	return a.chooseLabel(ctx, key, scr, label)
}

// chooseLabel applies a label chosen on a picker: at once to a card's
// torrent, and to a list's torrents after asking.
func (a *application) chooseLabel(ctx context.Context, key screenKey, scr *screen, label string) (string, bool) {
	if scr.bulk != nil {
		next, bulk := *scr, *scr.bulk
		bulk.action, bulk.label = bulkLabel, label
		next.bulk, next.picks = &bulk, nil
		return a.redrawBulk(ctx, key, &next, "")
	}
	torrent, err := a.rtorrent.GetTorrentContext(ctx, scr.hash)
	if err != nil {
		return err.Error(), true
	}
	done, err := a.applyLabel(ctx, torrent, label)
	if err != nil {
		return err.Error(), true
	}
	next := *scr
	next.confirm, next.picks = "", nil
	return a.redrawCard(ctx, key, &next, done)
}

// typedLabel applies a label typed in reply to a picker.
func (a *application) typedLabel(ctx context.Context, chatID int64, key screenKey, scr *screen, text string) {
	label, err := checkLabel(text)
	if err != nil {
		a.send(ctx, chatID, "setlabel: "+err.Error())
		return
	}
	done, failed := a.chooseLabel(ctx, key, scr, label)
	switch {
	case failed:
		a.send(ctx, chatID, "setlabel: "+done)
	case scr.bulk != nil:
		a.send(ctx, chatID, "Confirm on the message you replied to.")
	default:
		a.send(ctx, chatID, done)
	}
}
