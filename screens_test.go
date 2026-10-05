package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

const master = 7

// buttonApp is an authorized private chat with the bot, over a fake
// rTorrent holding torrents.
func buttonApp(t *testing.T, torrents rtapi.Torrents) (*application, *fakeTelegram, *fakeRtorrent) {
	t.Helper()
	rtorrentFake, client := newFakeRtorrent(t, torrents...)
	telegramFake := &fakeTelegram{sent: make(chan sentMessage, 16)}
	app := &application{
		bot: newTestBot(t, telegramFake, "123:SECRET"), rtorrent: client,
		logger: log.New(io.Discard, "", 0), token: "123:SECRET",
		masters: principals{ids: map[int64]struct{}{master: {}}}, botUsername: "ThisBot",
		noLive: true, state: &state{},
	}
	return app, telegramFake, rtorrentFake
}

func command(app *application, text string) {
	app.handle(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: master}, Chat: models.Chat{ID: 111, Type: models.ChatTypePrivate}, Text: text,
	}})
}

func press(app *application, from int64, messageID int, data string) {
	app.handle(context.Background(), &models.Update{CallbackQuery: &models.CallbackQuery{
		ID: "query", From: models.User{ID: from}, Data: data,
		Message: models.MaybeInaccessibleMessage{
			Type:    models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: messageID, Chat: models.Chat{ID: 111, Type: models.ChatTypePrivate}},
		},
	}})
}

func nextSent(t *testing.T, telegramFake *fakeTelegram) sentMessage {
	t.Helper()
	select {
	case message := <-telegramFake.sent:
		return message
	case <-time.After(2 * time.Second):
		t.Fatal("no message was sent")
		return sentMessage{}
	}
}

func lastEdit(t *testing.T, telegramFake *fakeTelegram) sentMessage {
	t.Helper()
	if len(telegramFake.edits) == 0 {
		t.Fatal("no message was edited")
	}
	return telegramFake.edits[len(telegramFake.edits)-1]
}

func lastAnswer(t *testing.T, telegramFake *fakeTelegram) callbackAnswer {
	t.Helper()
	if len(telegramFake.answers) == 0 {
		t.Fatal("the button press was not answered")
	}
	return telegramFake.answers[len(telegramFake.answers)-1]
}

// buttonTexts lists button labels row by row, as "label=data".
func buttonTexts(buttons [][]models.InlineKeyboardButton) []string {
	var texts []string
	for _, row := range buttons {
		for _, b := range row {
			texts = append(texts, b.Text+"="+b.CallbackData)
		}
	}
	return texts
}

func hasButton(buttons [][]models.InlineKeyboardButton, label string) bool {
	return slices.ContainsFunc(buttonTexts(buttons), func(text string) bool { return strings.HasPrefix(text, label+"=") })
}

func TestListsPageWithButtons(t *testing.T) {
	var torrents rtapi.Torrents
	for i := range 25 {
		torrents = append(torrents, &rtapi.Torrent{Name: fmt.Sprintf("torrent %02d", i+1), Hash: fmt.Sprintf("%02X%038X", i+1, 0), State: rtapi.Stopped})
	}
	app, telegramFake, _ := buttonApp(t, torrents)

	command(app, "list")
	first := nextSent(t, telegramFake)
	if !strings.HasSuffix(first.text, "Page 1 of 3, 25 torrents") || !strings.Contains(first.text, "torrent 10") || strings.Contains(first.text, "torrent 11") {
		t.Fatalf("first page = %q", first.text)
	}
	if got := len(first.buttons); got != 11 || hasButton(first.buttons, "◀") || !hasButton(first.buttons, "▶") || !hasButton(first.buttons, "📄 All") {
		t.Fatalf("first page buttons = %v", buttonTexts(first.buttons))
	}

	press(app, master, first.messageID, "pg:1")
	second := lastEdit(t, telegramFake)
	if second.messageID != first.messageID || !strings.Contains(second.text, "torrent 11") || !hasButton(second.buttons, "◀") || !hasButton(second.buttons, "▶") {
		t.Fatalf("second page = %q %v", second.text, buttonTexts(second.buttons))
	}

	press(app, master, first.messageID, "pg:2")
	last := lastEdit(t, telegramFake)
	if !strings.Contains(last.text, "torrent 25") || len(last.buttons) != 6 || hasButton(last.buttons, "▶") {
		t.Fatalf("last page = %q %v", last.text, buttonTexts(last.buttons))
	}

	press(app, master, first.messageID, "all")
	all := nextSent(t, telegramFake)
	if strings.Count(all.text, "torrent ") != 25 {
		t.Fatalf("full list = %q", all.text)
	}
}

func TestTorrentCardActsAndGoesBack(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	command(app, "list")
	list := nextSent(t, telegramFake)

	press(app, master, list.messageID, "t:"+strings.Repeat("B", 40))
	card := lastEdit(t, telegramFake)
	if !strings.Contains(card.text, "Ubuntu\nSeeding") {
		t.Fatalf("card = %q", card.text)
	}
	for _, label := range []string{"⏸ Stop", "🔍 Check", "🗑 Remove", "🔄 Refresh", "« Back"} {
		if !hasButton(card.buttons, label) {
			t.Fatalf("card is missing %s: %v", label, buttonTexts(card.buttons))
		}
	}
	if hasButton(card.buttons, "💣 Remove + data") || hasButton(card.buttons, "▶ Start") {
		t.Fatalf("card offers unavailable actions: %v", buttonTexts(card.buttons))
	}

	press(app, master, list.messageID, "a:stop")
	if calls := rtorrentFake.called("d.stop"); len(calls) != 1 || calls[0][0] != strings.Repeat("B", 40) {
		t.Fatalf("d.stop calls = %v", calls)
	}
	if answer := lastAnswer(t, telegramFake); answer.text != "Stopped" || answer.alert {
		t.Fatalf("answer = %+v", answer)
	}

	press(app, master, list.messageID, "back")
	if back := lastEdit(t, telegramFake); !strings.HasPrefix(back.text, "<aaa> Debian\n") {
		t.Fatalf("back = %q", back.text)
	}
}

func TestRemoveButtonAsksFirst(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	command(app, "list")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "t:"+strings.Repeat("E", 40))

	press(app, master, list.messageID, "a:del")
	ask := lastEdit(t, telegramFake)
	if !strings.HasPrefix(ask.text, "Remove Gentoo from rTorrent?") || !hasButton(ask.buttons, "✅ Remove") || !hasButton(ask.buttons, "Cancel") {
		t.Fatalf("confirmation = %q %v", ask.text, buttonTexts(ask.buttons))
	}
	press(app, master, list.messageID, "n")
	if cancelled := lastEdit(t, telegramFake); !strings.HasPrefix(cancelled.text, "Gentoo\nError") {
		t.Fatalf("cancel = %q", cancelled.text)
	}
	// A late press on the old confirmation, after Cancel, must not remove.
	press(app, master, list.messageID, "y:del")
	if calls := rtorrentFake.called("d.erase"); len(calls) != 0 {
		t.Fatalf("cancel or a stale confirmation erased: %v", calls)
	}
	if answer := lastAnswer(t, telegramFake); !answer.alert {
		t.Fatalf("stale confirmation = %+v", answer)
	}

	press(app, master, list.messageID, "a:del")
	press(app, master, list.messageID, "y:del")
	if calls := rtorrentFake.called("d.erase"); len(calls) != 1 {
		t.Fatalf("d.erase calls = %v", calls)
	}
	if after := lastEdit(t, telegramFake); strings.Contains(after.text, "Gentoo") || !strings.Contains(after.text, "Debian") {
		t.Fatalf("list after removal = %q", after.text)
	}
	if answer := lastAnswer(t, telegramFake); answer.text != "Removed: Gentoo" {
		t.Fatalf("answer = %+v", answer)
	}
}

// Removing the last torrent a list shows leaves the message saying what was
// removed, not that the search finds nothing.
func TestRemovingTheLastResultSaysSo(t *testing.T) {
	app, telegramFake, _ := buttonApp(t, handlerTorrents())
	command(app, "search gentoo")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "t:"+strings.Repeat("E", 40))
	press(app, master, list.messageID, "a:del")
	press(app, master, list.messageID, "y:del")
	if after := lastEdit(t, telegramFake); after.text != "Removed: Gentoo" || len(after.buttons) != 0 {
		t.Fatalf("message after removing the only match = %q %v", after.text, buttonTexts(after.buttons))
	}
	// Paging an emptied list still says it is empty.
	command(app, "search ubuntu")
	other := nextSent(t, telegramFake)
	command(app, "del bbb")
	nextSent(t, telegramFake)
	press(app, master, other.messageID, "pg:0")
	if after := lastEdit(t, telegramFake); after.text != "No matches" {
		t.Fatalf("emptied list = %q", after.text)
	}
}

func TestDeldataCommandAsksWithButtons(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "show")
	if err := os.MkdirAll(show, 0o700); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("A", 40)
	app, telegramFake, _ := buttonApp(t, rtapi.Torrents{{Name: "show", Hash: hash, Path: show}})
	app.dataRoot = root

	command(app, "deldata aaaaaaa")
	ask := nextSent(t, telegramFake)
	if !strings.HasPrefix(ask.text, "Remove show and delete its data from disk?") || !hasButton(ask.buttons, "💣 Delete data") {
		t.Fatalf("confirmation = %q %v", ask.text, buttonTexts(ask.buttons))
	}
	if _, err := os.Stat(show); err != nil {
		t.Fatalf("data deleted before confirmation: %v", err)
	}

	press(app, master, ask.messageID, "y:deldata")
	if done := lastEdit(t, telegramFake); done.text != "Deleted with data: show" || len(done.buttons) != 0 {
		t.Fatalf("result = %q %v", done.text, buttonTexts(done.buttons))
	}
	if _, err := os.Stat(show); !os.IsNotExist(err) {
		t.Fatalf("data still exists: %v", err)
	}
	press(app, master, ask.messageID, "y:deldata")
	if answer := lastAnswer(t, telegramFake); !answer.alert || !strings.Contains(answer.text, "expired") {
		t.Fatalf("second press = %+v", answer)
	}
}

func TestButtonsRefuseStrangersAndExpiredMessages(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	command(app, "info aaaaaaa")
	card := nextSent(t, telegramFake)

	press(app, 999, card.messageID, "a:stop")
	if answer := lastAnswer(t, telegramFake); !strings.Contains(answer.text, "owners") {
		t.Fatalf("stranger's press = %+v", answer)
	}
	press(app, master, card.messageID+100, "a:stop")
	if answer := lastAnswer(t, telegramFake); !answer.alert || !strings.Contains(answer.text, "expired") {
		t.Fatalf("expired press = %+v", answer)
	}
	if calls := rtorrentFake.called("d.stop"); len(calls) != 0 || len(telegramFake.edits) != 0 {
		t.Fatalf("refused presses acted: stops=%v edits=%d", calls, len(telegramFake.edits))
	}
}

func TestInfoCardRefreshes(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	command(app, "info bbbbbbb")
	card := nextSent(t, telegramFake)
	if !hasButton(card.buttons, "🔄 Refresh") || hasButton(card.buttons, "« Back") {
		t.Fatalf("info card buttons = %v", buttonTexts(card.buttons))
	}
	rtorrentFake.set(func(f *fakeRtorrent) { f.torrents[1].UpRate = 5 << 20 })
	press(app, master, card.messageID, "a:refresh")
	if refreshed := lastEdit(t, telegramFake); !strings.Contains(refreshed.text, "↑ 5.0 MiB") {
		t.Fatalf("refreshed card = %q", refreshed.text)
	}
	if answer := lastAnswer(t, telegramFake); answer.text != "Updated" {
		t.Fatalf("answer = %+v", answer)
	}
}
