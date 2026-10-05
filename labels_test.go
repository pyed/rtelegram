package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

// replyTo sends text as a master's reply to the bot's message messageID.
func replyTo(app *application, messageID int, text string) {
	app.handle(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: master}, Chat: models.Chat{ID: 111, Type: models.ChatTypePrivate}, Text: text,
		ReplyToMessage: &models.Message{ID: messageID},
	}})
}

// labelledTorrents have labels as ruTorrent stores them.
func labelledTorrents() rtapi.Torrents {
	hash := func(c string) string { return strings.Repeat(c, 40) }
	return rtapi.Torrents{
		{Name: "Alien", Hash: hash("A"), State: rtapi.Seeding, Label: "Movies"},
		{Name: "Brazil", Hash: hash("B"), State: rtapi.Seeding, Label: "Movies"},
		{Name: "Cosmos", Hash: hash("C"), State: rtapi.Stopped, Label: "TV%20Shows"},
		{Name: "Dune", Hash: hash("D"), State: rtapi.Seeding},
	}
}

func labelOfFake(rtorrentFake *fakeRtorrent, index int) string {
	var label string
	rtorrentFake.set(func(f *fakeRtorrent) { label = f.torrents[index].Label })
	return label
}

// Labels are stored as ruTorrent stores them: percent-encoded as
// JavaScript's encodeURIComponent does, and shown decoded.
func TestLabelsAreStoredAsRuTorrentStoresThem(t *testing.T) {
	for label, encoded := range map[string]string{
		"Movies":              "Movies",
		"TV Shows":            "TV%20Shows",
		"Séries/4K & more":    "S%C3%A9ries%2F4K%20%26%20more",
		"a-b_c.d!e~f*g'h(i)j": "a-b_c.d!e~f*g'h(i)j",
		"100%":                "100%25",
	} {
		if got := encodeLabel(label); got != encoded {
			t.Errorf("encodeLabel(%q) = %q, want %q", label, got, encoded)
		}
		if got := labelOf(&rtapi.Torrent{Label: encoded}); got != label {
			t.Errorf("labelOf(%q) = %q, want %q", encoded, got, label)
		}
	}
	// Labels other programs set without encoding them read as they are.
	for raw, want := range map[string]string{"50% done": "50% done", " spaced%20out ": "spaced out", "bad%FF": "bad%FF", "bad\xff": "bad�", "": ""} {
		if got := labelOf(&rtapi.Torrent{Label: raw}); got != want {
			t.Errorf("labelOf(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestLabelsListsEachLabelAndItsTorrents(t *testing.T) {
	app, telegramFake, _ := buttonApp(t, labelledTorrents())

	command(app, "labels")
	labels := nextSent(t, telegramFake)
	if labels.text != "Labels\nMovies: 2\nTV Shows: 1\nNo label: 1\n\nTap a label to list its torrents." {
		t.Fatalf("labels = %q", labels.text)
	}
	if got := strings.Join(buttonTexts(labels.buttons), ","); got != "Movies (2)=lo:0,TV Shows (1)=lo:1,No label (1)=lo:2" {
		t.Fatalf("label buttons = %s", got)
	}
	press(app, master, labels.messageID, "lo:1")
	if list := lastEdit(t, telegramFake); !strings.Contains(list.text, "Cosmos") || strings.Contains(list.text, "Alien") {
		t.Fatalf("TV Shows = %q", list.text)
	}
	command(app, "labels")
	labels = nextSent(t, telegramFake)
	for _, stale := range []string{"lo:3", "lo:-1", "lo:x"} {
		press(app, master, labels.messageID, stale)
		if answer := lastAnswer(t, telegramFake); !answer.alert {
			t.Fatalf("%s = %+v", stale, answer)
		}
	}

	for text, want := range map[string][]string{
		"labels movies":   {"Alien", "Brazil"},
		"labels TV Shows": {"Cosmos"},
		"labels -":        {"Dune"},
	} {
		command(app, text)
		list := nextSent(t, telegramFake)
		for _, name := range []string{"Alien", "Brazil", "Cosmos", "Dune"} {
			if strings.Contains(list.text, name) != strings.Contains(strings.Join(want, ","), name) {
				t.Errorf("%s = %q, want %v", text, list.text, want)
			}
		}
	}
	if got := lastSentText(t, telegramFake, app, "labels Music"); got != "No torrent has this label" {
		t.Errorf("labels Music = %q", got)
	}
}

func TestLabelsWithoutAnyLabelSaysHowToSetOne(t *testing.T) {
	app, telegramFake, _ := buttonApp(t, handlerTorrents())
	if got := lastSentText(t, telegramFake, app, "labels"); !strings.HasPrefix(got, "No torrent has a label.") {
		t.Fatalf("labels = %q", got)
	}
}

func TestSetLabelSetsAndRemovesLabels(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, labelledTorrents())
	for text, want := range map[string]string{
		"setlabel d Kids & Family": "Labelled Dune: Kids & Family",
		"setlabel":                 "setlabel: use setlabel HASH LABEL, or setlabel HASH - to remove the label",
		"setlabel z Movies":        `setlabel: no torrent matches hash prefix "z"`,
	} {
		if got := lastSentText(t, telegramFake, app, text); got != want {
			t.Errorf("%s = %q, want %q", text, got, want)
		}
	}
	if got := labelOfFake(rtorrentFake, 3); got != "Kids%20%26%20Family" {
		t.Fatalf("stored label = %q", got)
	}
	if got := lastSentText(t, telegramFake, app, "setlabel d -"); got != "Removed the label of Dune" || labelOfFake(rtorrentFake, 3) != "" {
		t.Fatalf("setlabel d - = %q, label %q", got, labelOfFake(rtorrentFake, 3))
	}
	if got := lastSentText(t, telegramFake, app, "setlabel d "+strings.Repeat("x", maxLabelRunes+1)); !strings.HasPrefix(got, "setlabel: a label has at most") {
		t.Fatalf("a long label = %q", got)
	}
}

// A card's label button offers the labels in use, the most used first, and
// one that removes the label; the picker takes a new label as a reply.
func TestCardsPickLabels(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, labelledTorrents())
	command(app, "info c")
	card := nextSent(t, telegramFake)
	if !strings.Contains(card.text, "\nLabel: TV Shows") || !hasButton(card.buttons, "🏷 Label") {
		t.Fatalf("card = %q %v", card.text, buttonTexts(card.buttons))
	}
	press(app, master, card.messageID, "a:label")
	picker := lastEdit(t, telegramFake)
	if got := strings.Join(buttonTexts(picker.buttons), ","); got != "🏷 Movies=lb:0,🏷 TV Shows=lb:1,✖ No label=lb:-,« Back=n" ||
		!strings.Contains(picker.text, "Its label is TV Shows.") {
		t.Fatalf("picker = %q %s", picker.text, got)
	}
	press(app, master, card.messageID, "lb:0")
	if redrawn := lastEdit(t, telegramFake); labelOfFake(rtorrentFake, 2) != "Movies" || !strings.Contains(redrawn.text, "\nLabel: Movies") ||
		lastAnswer(t, telegramFake).text != "Labelled Cosmos: Movies" {
		t.Fatalf("after choosing = %q, label %q, answer %+v", redrawn.text, labelOfFake(rtorrentFake, 2), lastAnswer(t, telegramFake))
	}
	for _, stale := range []string{"lb:0", "lb:-"} {
		press(app, master, card.messageID, stale)
		if answer := lastAnswer(t, telegramFake); !answer.alert || labelOfFake(rtorrentFake, 2) != "Movies" {
			t.Fatalf("%s on the card = %+v", stale, answer)
		}
	}

	press(app, master, card.messageID, "a:label")
	press(app, master, card.messageID, "n")
	if back := lastEdit(t, telegramFake); !strings.HasPrefix(back.text, "Cosmos\n") || !hasButton(back.buttons, "🏷 Label") {
		t.Fatalf("back from the picker = %q", back.text)
	}

	press(app, master, card.messageID, "a:label")
	replyTo(app, card.messageID, "  Documentaries  ")
	if got := nextSent(t, telegramFake).text; got != "Labelled Cosmos: Documentaries" || labelOfFake(rtorrentFake, 2) != "Documentaries" {
		t.Fatalf("a reply = %q, label %q", got, labelOfFake(rtorrentFake, 2))
	}
	// Once the label is chosen, replies are ordinary messages again.
	replyTo(app, card.messageID, "Other")
	if got := nextSent(t, telegramFake).text; got != "no such command, try /help" {
		t.Fatalf("a reply to a card = %q", got)
	}

	command(app, "setlabel d")
	picker = nextSent(t, telegramFake)
	if !strings.HasPrefix(picker.text, "Choose a label for Dune") || hasButton(picker.buttons, "✖ No label") {
		t.Fatalf("setlabel d = %q %v", picker.text, buttonTexts(picker.buttons))
	}
	replyTo(app, picker.messageID, "two\nlines")
	if got := nextSent(t, telegramFake).text; got != "setlabel: a label is one line" || labelOfFake(rtorrentFake, 3) != "" {
		t.Fatalf("a two-line reply = %q", got)
	}
	// A command in reply is a command.
	replyTo(app, picker.messageID, "/labels")
	if got := nextSent(t, telegramFake).text; !strings.HasPrefix(got, "Labels\n") || labelOfFake(rtorrentFake, 3) != "" {
		t.Fatalf("a command in reply = %q, label %q", got, labelOfFake(rtorrentFake, 3))
	}
	press(app, master, picker.messageID, "lb:-")
	if answer := lastAnswer(t, telegramFake); answer.alert || labelOfFake(rtorrentFake, 3) != "" {
		t.Fatalf("no label on an unlabelled torrent = %+v", answer)
	}
}

// Labels given when adding, in an upload's caption or a watch rule, are
// stored encoded, so ruTorrent shows them.
func TestLabelsGivenWhenAddingAreEncoded(t *testing.T) {
	file := []byte("d4:infod4:name4:testee")
	app, telegramFake, rtorrentFake := buttonApp(t, nil)
	telegramFake.file = file
	app.httpClient = telegramFake
	app.addTimeout = 50 * time.Millisecond
	app.addPollInterval = 10 * time.Millisecond
	message := &models.Message{
		Chat:     models.Chat{ID: 111, Type: models.ChatTypePrivate},
		Document: &models.Document{FileID: "file", FileName: "a.torrent", FileSize: int64(len(file))},
	}
	app.receiveTorrent(context.Background(), 111, message, "l=Séries")
	app.wg.Wait()

	ix := newFakeIndexer(t)
	app.indexer = newIndexer(ix.server.URL+"/api", indexerKey)
	app.downloadRoot = "/downloads"
	loadByName(rtorrentFake, "e1")
	command(app, "watch add tv show l=Séries")
	app.checkWatches(context.Background())
	ix.setItems("show", torznabItem("Show e1", magnetFor("e1"), 1<<30, 9))
	app.checkWatches(context.Background())
	app.wg.Wait()

	loads := rtorrentFake.loadCalls()
	if len(loads) != 2 {
		t.Fatalf("loads = %q", loads)
	}
	for _, load := range loads {
		if !strings.Contains(strings.Join(load, " "), `d.custom1.set="S%C3%A9ries"`) {
			t.Errorf("load without the encoded label: %q", load)
		}
	}
}

func TestDigestsCountWhatFinishedAndWasAddedByLabel(t *testing.T) {
	if got := labelBreakdown(labelledTorrents()); got != " (Movies 2, TV Shows 1, no label 1)" {
		t.Fatalf("breakdown = %q", got)
	}
	if got := labelBreakdown(handlerTorrents()); got != "" {
		t.Fatalf("breakdown without labels = %q", got)
	}
	var many rtapi.Torrents
	for _, label := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		many = append(many, &rtapi.Torrent{Label: label})
	}
	if got := labelBreakdown(many); !strings.HasSuffix(got, ", h 1, 2 more labels)") {
		t.Fatalf("breakdown of many = %q", got)
	}
}
