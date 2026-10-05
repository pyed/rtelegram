package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pyed/rtapi"
)

const unregisteredMessage = `Tracker: [Failure reason "Unregistered torrent"]`

// Only a tracker's refusal that says it no longer has a torrent counts, and
// only for private torrents; trouble reaching a tracker, and refusals about
// the account or client, never do.
func TestOnlyDeletionsCountAsUnregistered(t *testing.T) {
	for message, want := range map[string]bool{
		unregisteredMessage: true,
		`Tracker: [Failure reason "unregistered torrent"]`:                             true,
		`Tracker: [Failure reason "Torrent not registered with this tracker."]`:        true,
		`Tracker: [Failure reason "Torrent is not registered"]`:                        true,
		`Tracker: [Failure reason "InfoHash not found."]`:                              true,
		`Tracker: [Failure reason "info_hash not found"]`:                              true,
		`Tracker: [Failure reason "Torrent not found"]`:                                true,
		`Tracker: [Failure reason "Torrent has been deleted."]`:                        true,
		`Tracker: [Failure reason "Torrent was removed"]`:                              true,
		`Tracker: [Failure reason "Unknown torrent"]`:                                  true,
		`Tracker: [Failure reason "Torrent does not exist"]`:                           true,
		`Tracker: [Failure reason "Unregistered torrent: trumped by a later release"]`: true,
		`Tracker: [Failure reason "Trumped"]`:                                          true,
		`Tracker: [received error message: unregistered torrent]`:                      true,
		`Tracker: [Received error message: Torrent not found]`:                         true,
		`Tracker: [tracker message: unregistered torrent]`:                             true,
		`Tracker: [Tracker warning: Torrent cannot be found]`:                          true,

		`Tracker: [Timeout was reached]`:                                        false,
		`Tracker: [Could not resolve host: tracker.example.org]`:                false,
		`Tracker: [Couldn't connect to server]`:                                 false,
		`Tracker: [The requested URL returned error: 404]`:                      false,
		`Tracker: [Could not parse bencoded data: <h1>Unregistered torrent]`:    false,
		`Tracker: [Failure reason "Invalid passkey"]`:                           false,
		`Tracker: [Failure reason "Passkey not registered"]`:                    false,
		`Tracker: [Failure reason "Your client is not registered"]`:             false,
		`Tracker: [Failure reason "Unregistered IP address"]`:                   false,
		`Tracker: [Failure reason "Unregistered user"]`:                         false,
		`Tracker: [Failure reason "Account not found"]`:                         false,
		`Tracker: [Failure reason "Torrent is pending moderation"]`:             false,
		`Tracker: [Failure reason "Torrent not found, tracker in maintenance"]`: false,
		`Tracker: [Failure reason "Torrent not found; too many requests"]`:      false,
		`Tracker: [Failure reason "You are banned"]`:                            false,
		`Tracker: [Failure reason "Unregistered torrent"`:                       false,
		"Unregistered torrent":                                                  false,
		"":                                                                      false,
	} {
		if got := unregistered(&rtapi.Torrent{Message: message, Private: true}); got != want {
			t.Errorf("unregistered(%q) = %v, want %v", message, got, want)
		}
		if unregistered(&rtapi.Torrent{Message: message}) {
			t.Errorf("a public torrent with %q counts", message)
		}
	}
}

func unregisteredTorrents() rtapi.Torrents {
	hash := func(c string) string { return strings.Repeat(c, 40) }
	return rtapi.Torrents{
		{Name: "gone", Hash: hash("A"), State: rtapi.Error, Message: unregisteredMessage, Private: true},
		{Name: "unreachable", Hash: hash("B"), State: rtapi.Error, Message: "Tracker: [Timeout was reached]", Private: true},
		{Name: "public", Hash: hash("C"), State: rtapi.Error, Message: unregisteredMessage},
		{Name: "stopped and gone", Hash: hash("D"), State: rtapi.Stopped, Message: unregisteredMessage, Private: true},
		{Name: "fine", Hash: hash("E"), State: rtapi.Seeding, Private: true},
	}
}

func TestUnregisteredListsThemWithButtonsThatRemoveThemAll(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, unregisteredTorrents())
	command(app, "unregistered")
	list := nextSent(t, telegramFake)
	if !strings.Contains(list.text, "> gone\n"+unregisteredMessage) || !strings.Contains(list.text, "> stopped and gone\n") ||
		strings.Contains(list.text, "unreachable") || strings.Contains(list.text, "public") {
		t.Fatalf("unregistered = %q", list.text)
	}
	if got := strings.Join(buttonTexts(list.buttons), ","); got != "gone=t:"+strings.Repeat("A", 40)+",stopped and gone=t:"+strings.Repeat("D", 40)+",🗑 Remove all 2=bk:del" {
		t.Fatalf("buttons without -data-root = %s", got)
	}
	press(app, master, list.messageID, "un")
	if answer := lastAnswer(t, telegramFake); !answer.alert {
		t.Fatalf("un on the unregistered list itself = %+v", answer)
	}
	press(app, master, list.messageID, "bk:"+bulkDel)
	confirm := lastEdit(t, telegramFake)
	if confirm.text != "Remove 2 torrents their tracker no longer has from rTorrent? Their data stays on disk.\n• gone\n• stopped and gone" {
		t.Fatalf("confirmation = %q", confirm.text)
	}
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if erased := strings.Join(hashesCalled(rtorrentFake, "d.erase"), ""); erased != "AD" {
		t.Fatalf("removed %q", erased)
	}
	if got := lastSentText(t, telegramFake, app, "unregistered"); got != "No unregistered torrents" {
		t.Fatalf("after removing = %q", got)
	}

	app, telegramFake, _ = buttonApp(t, unregisteredTorrents())
	app.dataRoot = t.TempDir()
	command(app, "unregistered")
	if list := nextSent(t, telegramFake); !hasButton(list.buttons, "💣 Remove all 2 + data") {
		t.Fatalf("buttons with -data-root = %v", buttonTexts(list.buttons))
	}
}

// The errors list, and cards, point out unregistered torrents.
func TestErrorsAndCardsPointOutUnregisteredTorrents(t *testing.T) {
	app, telegramFake, _ := buttonApp(t, unregisteredTorrents())
	command(app, "errors")
	list := nextSent(t, telegramFake)
	if !hasButton(list.buttons, "🧹 1 unregistered") {
		t.Fatalf("errors buttons = %v", buttonTexts(list.buttons))
	}
	press(app, master, list.messageID, "un")
	if shown := nextSent(t, telegramFake); !strings.Contains(shown.text, "gone") || strings.Contains(shown.text, "unreachable") {
		t.Fatalf("unregistered from errors = %q", shown.text)
	}

	command(app, "info a")
	if card := nextSent(t, telegramFake); !strings.HasSuffix(card.text, "\n"+unregisteredMessage+"\nIts tracker no longer has it.") {
		t.Fatalf("card = %q", card.text)
	}
	command(app, "info b")
	if card := nextSent(t, telegramFake); !strings.HasSuffix(card.text, "\nTracker: [Timeout was reached]") {
		t.Fatalf("card of an unreachable tracker's torrent = %q", card.text)
	}

	app, telegramFake, _ = buttonApp(t, handlerTorrents())
	command(app, "errors")
	if list := nextSent(t, telegramFake); hasButton(list.buttons, "🧹 0 unregistered") || len(list.buttons) != 1 {
		t.Fatalf("errors without unregistered torrents = %v", buttonTexts(list.buttons))
	}
}

func TestErrorNotificationsOfferCleaningUpUnregisteredTorrents(t *testing.T) {
	tracker, _ := url.Parse("https://tracker.example/announce")
	var torrents rtapi.Torrents
	for i := range 5 {
		torrents = append(torrents, &rtapi.Torrent{Name: fmt.Sprint("seed ", i), Hash: fmt.Sprintf("%040X", i+1), State: rtapi.Seeding, Tracker: tracker, Private: true})
	}
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	subscribe(t, app, 111, eventErrors)
	w := &watcher{}
	app.checkEvents(context.Background(), w, time.Now())
	rtorrentFake.set(func(f *fakeRtorrent) {
		for _, torrent := range f.torrents[:4] {
			torrent.State, torrent.Message = rtapi.Error, unregisteredMessage
		}
	})
	app.checkEvents(context.Background(), w, time.Now())
	message := nextSent(t, telegramFake)
	if !strings.HasPrefix(message.text, "⚠️ Errors: 4 torrents") || strings.Join(buttonTexts(message.buttons), ",") != "🧹 4 unregistered: clean up=un" {
		t.Fatalf("notification = %q %v", message.text, buttonTexts(message.buttons))
	}
	press(app, master, message.messageID, "un")
	if list := nextSent(t, telegramFake); !strings.Contains(list.text, "seed 3") || !hasButton(list.buttons, "🗑 Remove all 4") {
		t.Fatalf("list from the notification = %q %v", list.text, buttonTexts(list.buttons))
	}
}

func TestDigestsCountUnregisteredAndLabelledTorrents(t *testing.T) {
	torrents := unregisteredTorrents()
	torrents[0].Label, torrents[1].Label = "Movies", "Movies"
	for _, torrent := range torrents {
		torrent.Started = uint64(time.Now().Unix())
	}
	app, _, _ := buttonApp(t, torrents)
	text, _, err := app.buildDigest(context.Background(), time.Now(), digestSettings{Time: "08:00"})
	if err != nil || !strings.Contains(text, "\nUnregistered: 2, which /unregistered removes") || !strings.Contains(text, "\nAdded: 5 (Movies 2, no label 3)") {
		t.Fatalf("digest = %q, %v", text, err)
	}
	app, _, _ = buttonApp(t, handlerTorrents())
	if text, _, _ := app.buildDigest(context.Background(), time.Now(), digestSettings{Time: "08:00"}); strings.Contains(text, "Unregistered") {
		t.Fatalf("digest without unregistered torrents = %q", text)
	}
}
