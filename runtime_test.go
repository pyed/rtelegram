package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

// groupCommand sends text to the bot from the master, in a group's topic.
func groupCommand(app *application, chatID int64, thread int, text string) {
	app.handle(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: master}, Chat: models.Chat{ID: chatID, Type: models.ChatTypeSupergroup},
		MessageThreadID: thread, Text: text,
	}})
}

// A group that becomes a supergroup gets a new ID. Telegram answers sends to
// the old one with the new one, and what the bot keeps for the chat moves
// there.
func TestAGroupThatBecameASupergroupKeepsItsSettings(t *testing.T) {
	const group, supergroup = -100, -1001234567890
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	app.now = func() time.Time { return day(1, 10, 0) }
	ctx := context.Background()
	for _, text := range []string{"/digest 08:00", "/notify on", "/sort rev name"} {
		groupCommand(app, group, 0, text)
	}
	// The supergroup's own sort order is kept.
	if err := app.state.update(func(data *stateData) {
		data.Watch = []watchRule{{Name: "news", Query: "news", ChatID: group}}
		data.Sorts[supergroup] = rtapi.ByUpTotal
	}); err != nil {
		t.Fatal(err)
	}
	drain(telegramFake)
	telegramFake.migrated = map[int64]int64{group: supergroup}

	w := &watcher{}
	app.checkEvents(ctx, w, time.Now())
	rtorrentFake.set(func(f *fakeRtorrent) { f.torrents[1].Finished = 9000 })
	app.checkEvents(ctx, w, time.Now())
	if message := nextSent(t, telegramFake); message.chatID != supergroup || !hasButton(message.buttons, "ℹ Details") {
		t.Fatalf("notification = %+v", message)
	}
	app.state.read(func(data *stateData) {
		_, notifyOld := data.Notify[group]
		_, digestOld := data.Digest[group]
		_, sortOld := data.Sorts[group]
		_, digestNew := data.Digest[supergroup]
		if notifyOld || digestOld || sortOld || len(data.Notify[supergroup].Events) == 0 || !digestNew ||
			data.Sorts[supergroup] != rtapi.ByUpTotal || data.Watch[0].ChatID != supergroup {
			t.Fatalf("state after the move = %+v", *data)
		}
	})

	app.checkDigest(ctx, day(2, 8, 0))
	app.checkDigest(ctx, day(2, 8, 1))
	if message := nextSent(t, telegramFake); message.chatID != supergroup || !strings.HasPrefix(message.text, "📰") {
		t.Fatalf("digest = %+v", message)
	}
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("the digest came again: %q", sent)
	}
}

// A digest that finds its group has become a supergroup goes there, once.
func TestADigestFollowsAGroupThatBecameASupergroup(t *testing.T) {
	const group, supergroup = -200, -1009876543210
	app, telegramFake, _ := buttonApp(t, nil)
	app.now = func() time.Time { return day(1, 10, 0) }
	groupCommand(app, group, 0, "/digest 08:00")
	drain(telegramFake)
	telegramFake.migrated = map[int64]int64{group: supergroup}
	for _, at := range []time.Time{day(2, 8, 0), day(2, 8, 1)} {
		app.checkDigest(context.Background(), at)
	}
	if message := nextSent(t, telegramFake); message.chatID != supergroup || !strings.HasPrefix(message.text, "📰") {
		t.Fatalf("digest = %+v", message)
	}
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("the digest came again: %q", sent)
	}
}

// When a forum topic is deleted, what went to it goes to the chat itself.
func TestATopicThatIsGoneIsForgotten(t *testing.T) {
	const forum = -100
	app, telegramFake, _ := buttonApp(t, nil)
	app.now = func() time.Time { return day(1, 10, 0) }
	groupCommand(app, forum, 42, "/digest 08:00")
	groupCommand(app, forum, 42, "/notify on")
	drain(telegramFake)
	telegramFake.goneTopics = map[int]bool{42: true}

	app.checkDigest(context.Background(), day(2, 8, 0))
	if message := nextSent(t, telegramFake); message.chatID != forum || message.threadID != 0 || !strings.HasPrefix(message.text, "📰") {
		t.Fatalf("digest = %+v", message)
	}
	app.state.read(func(data *stateData) {
		if data.Digest[forum].Thread != 0 || data.Notify[forum].Thread != 0 {
			t.Fatalf("the topic was kept: %+v, %+v", data.Digest[forum], data.Notify[forum])
		}
	})
}

// A digest Telegram refuses is not retried every minute; the next one covers
// its time too.
func TestARefusedDigestWaitsForItsNextTime(t *testing.T) {
	app, telegramFake, _ := buttonApp(t, nil)
	app.now = func() time.Time { return day(1, 10, 0) }
	ctx := context.Background()
	lastSentText(t, telegramFake, app, "digest 08:00")
	telegramFake.refused = map[int64]string{111: "Bad Request: not enough rights to send text messages to the chat"}
	attempts := telegramFake.sendAttempts
	for _, at := range []time.Time{day(2, 8, 0), day(2, 8, 1), day(2, 20, 0)} {
		app.checkDigest(ctx, at)
	}
	if tries := telegramFake.sendAttempts - attempts; tries != 1 {
		t.Fatalf("tried to send the digest %d times, want once", tries)
	}
	telegramFake.refused = nil
	app.checkDigest(ctx, day(3, 8, 0))
	if sent := drain(telegramFake); len(sent) != 1 || !strings.Contains(sent[0], "\nSince Thu 1 Oct 10:00\n") {
		t.Fatalf("the next digest = %q", sent)
	}
}

// A chat that no longer exists loses its notifications and digest.
func TestAChatThatIsGoneLosesItsSubscriptions(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	app.now = func() time.Time { return day(1, 10, 0) }
	ctx := context.Background()
	lastSentText(t, telegramFake, app, "digest 08:00")
	subscribe(t, app, 111, eventCompleted)
	telegramFake.refused = map[int64]string{111: "Bad Request: chat not found"}

	w := &watcher{}
	app.checkEvents(ctx, w, time.Now())
	rtorrentFake.set(func(f *fakeRtorrent) { f.torrents[1].Finished = 9000 })
	app.checkEvents(ctx, w, time.Now())
	app.checkDigest(ctx, day(2, 8, 0))
	app.state.read(func(data *stateData) {
		if len(data.Notify) != 0 || len(data.Digest) != 0 {
			t.Fatalf("subscriptions of a chat that is gone: %+v, %+v", data.Notify, data.Digest)
		}
	})
}

func TestMigratedToReadsTheNewChat(t *testing.T) {
	if got := migratedTo(&telegram.MigrateError{Message: "bad request", MigrateToChatID: -100123}); got != -100123 {
		t.Errorf("from the library's error: %d", got)
	}
	// Where int has 32 bits, the library cannot decode the ID and quotes
	// Telegram's answer.
	decodeFailure := errors.New(`error decode response body for method sendMessage, {"ok":false,"error_code":400,` +
		`"parameters":{"migrate_to_chat_id":-1001234567890}}, json: cannot unmarshal number -1001234567890 into Go struct field of type int`)
	if got := migratedTo(decodeFailure); got != -1001234567890 {
		t.Errorf("from a decoding failure: %d", got)
	}
	if got := migratedTo(errors.New("bad request, Bad Request: chat not found")); got != 0 {
		t.Errorf("from another error: %d", got)
	}
}
