package main

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

func TestParseSize(t *testing.T) {
	for text, want := range map[string]uint64{
		"1024": 1024, "500K": 500 << 10, "5M": 5 << 20, "1.5G": 3 << 29, "5MB": 5 << 20,
		"5MiB": 5 << 20, "2t": 2 << 40, " 8m ": 8 << 20, "0": 0,
	} {
		if got, err := parseSize(text); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"", "abc", "-1", "5X", "1e400", "M"} {
		if got, err := parseSize(text); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", text, got)
		}
	}
}

// subscribe subscribes chat 111 to events without the /notify screen.
func subscribe(t *testing.T, app *application, chatID int64, events ...string) {
	t.Helper()
	if err := app.state.update(func(data *stateData) {
		if data.Notify == nil {
			data.Notify = make(map[int64]notifySettings)
		}
		data.Notify[chatID] = notifySettings{Events: events}
	}); err != nil {
		t.Fatal(err)
	}
}

// drain returns the texts of every message sent so far.
func drain(telegramFake *fakeTelegram) []string {
	var texts []string
	for {
		select {
		case message := <-telegramFake.sent:
			texts = append(texts, message.text)
		default:
			return texts
		}
	}
}

func TestCompletedTorrentsAreAnnouncedOnce(t *testing.T) {
	torrents := rtapi.Torrents{
		{Name: "old", Hash: strings.Repeat("A", 40), State: rtapi.Seeding, Size: 10, Completed: 10, Finished: 1000},
		{Name: "new", Hash: strings.Repeat("B", 40), State: rtapi.Leeching, Size: 2 << 30, Completed: 1},
	}
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	subscribe(t, app, 111, eventCompleted)
	ctx := context.Background()

	w := &watcher{}
	app.checkEvents(ctx, w, time.Now())
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("first check announced existing torrents: %q", sent)
	}

	rtorrentFake.set(func(f *fakeRtorrent) {
		f.torrents[1].State, f.torrents[1].Completed, f.torrents[1].Finished = rtapi.Seeding, 2<<30, 2000
	})
	app.checkEvents(ctx, w, time.Now())
	message := nextSent(t, telegramFake)
	if message.text != "✅ Completed: new\n2.0 GiB, ratio 0.00" || !hasButton(message.buttons, "ℹ Details") {
		t.Fatalf("announcement = %q %v", message.text, buttonTexts(message.buttons))
	}
	app.checkEvents(ctx, w, time.Now())
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("announced twice: %q", sent)
	}

	// Another torrent finishing in the same second is still announced.
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.torrents = append(f.torrents, &rtapi.Torrent{Name: "same second", Hash: strings.Repeat("C", 40), State: rtapi.Seeding, Finished: 2000})
	})
	app.checkEvents(ctx, w, time.Now())
	if sent := drain(telegramFake); len(sent) != 1 || !strings.HasPrefix(sent[0], "✅ Completed: same second") {
		t.Fatalf("same-second completion = %q", sent)
	}

	// After a restart, only completions since the last check are announced.
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.torrents = append(f.torrents, &rtapi.Torrent{Name: "while offline", Hash: strings.Repeat("D", 40), State: rtapi.Seeding, Finished: 3000})
	})
	app.checkEvents(ctx, &watcher{}, time.Now())
	if sent := drain(telegramFake); len(sent) != 1 || !strings.HasPrefix(sent[0], "✅ Completed: while offline") {
		t.Fatalf("after restart = %q", sent)
	}
}

func TestNewErrorsAreAnnouncedOnce(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	subscribe(t, app, 111, eventErrors)
	ctx := context.Background()
	w := &watcher{}

	app.checkEvents(ctx, w, time.Now())
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("existing errors were announced: %q", sent)
	}
	setError := func(message string) {
		rtorrentFake.set(func(f *fakeRtorrent) {
			f.torrents[0].State, f.torrents[0].Message = rtapi.Error, message
			if message == "" {
				f.torrents[0].State = rtapi.Leeching
			}
		})
	}
	setError("Tracker: [Failure reason \"unregistered torrent\"]")
	app.checkEvents(ctx, w, time.Now())
	app.checkEvents(ctx, w, time.Now())
	if sent := drain(telegramFake); len(sent) != 1 || sent[0] != "⚠️ Error: Debian\nTracker: [Failure reason \"unregistered torrent\"]" {
		t.Fatalf("error announcements = %q", sent)
	}
	setError("")
	app.checkEvents(ctx, w, time.Now())
	setError("Tracker: [Timeout was reached]")
	app.checkEvents(ctx, w, time.Now())
	if sent := drain(telegramFake); len(sent) != 1 || !strings.HasSuffix(sent[0], "Timeout was reached]") {
		t.Fatalf("a recurring error was not announced again: %q", sent)
	}
}

func TestStalledDownloadsAreAnnouncedOnce(t *testing.T) {
	torrents := rtapi.Torrents{{Name: "slow", Hash: strings.Repeat("A", 40), State: rtapi.Leeching, Size: 1000, Completed: 100}}
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	app.stallAfter = 30 * time.Minute
	subscribe(t, app, 111, eventStalled)
	ctx := context.Background()
	w := &watcher{}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	for _, minutes := range []int{0, 29} {
		app.checkEvents(ctx, w, start.Add(time.Duration(minutes)*time.Minute))
	}
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("announced before stallAfter: %q", sent)
	}
	app.checkEvents(ctx, w, start.Add(31*time.Minute))
	app.checkEvents(ctx, w, start.Add(40*time.Minute))
	if sent := drain(telegramFake); len(sent) != 1 || sent[0] != "🐢 Stalled: slow\nNo progress for 31m0s, at 10.0%" {
		t.Fatalf("stall announcements = %q", sent)
	}

	rtorrentFake.set(func(f *fakeRtorrent) { f.torrents[0].Completed = 200 })
	app.checkEvents(ctx, w, start.Add(41*time.Minute))
	app.checkEvents(ctx, w, start.Add(72*time.Minute))
	if sent := drain(telegramFake); len(sent) != 1 || !strings.HasPrefix(sent[0], "🐢 Stalled: slow") {
		t.Fatalf("a new stall after progress = %q", sent)
	}
}

func TestLowDiskSpaceIsAnnouncedWithHysteresis(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	app.lowDisk = 5 << 30
	subscribe(t, app, 111, eventDisk)
	ctx := context.Background()
	w := &watcher{}

	for _, free := range []uint64{3 << 30, 3 << 30, 5<<30 + 100<<20, 4 << 30} {
		rtorrentFake.set(func(f *fakeRtorrent) { f.freeSpace = free })
		app.checkEvents(ctx, w, time.Now())
	}
	if sent := drain(telegramFake); len(sent) != 1 || sent[0] != "💾 Low disk space: 3.0 GiB free where rTorrent saves data." {
		t.Fatalf("low disk announcements = %q", sent)
	}
	for _, free := range []uint64{6 << 30, 2 << 30} {
		rtorrentFake.set(func(f *fakeRtorrent) { f.freeSpace = free })
		app.checkEvents(ctx, w, time.Now())
	}
	if sent := drain(telegramFake); len(sent) != 1 || !strings.Contains(sent[0], "2.0 GiB free") {
		t.Fatalf("after recovery = %q", sent)
	}

	app.lowDisk = 0
	before := len(rtorrentFake.called("d.free_diskspace"))
	app.checkEvents(ctx, w, time.Now())
	if after := len(rtorrentFake.called("d.free_diskspace")); after != before {
		t.Fatal("disk space was checked with -low-disk 0")
	}
}

func TestEventsGoOnlyToSubscribedChats(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	subscribe(t, app, 111, eventErrors)
	subscribe(t, app, 222, eventCompleted, eventErrors)
	subscribe(t, app, 333, eventCompleted)
	if err := app.state.update(func(data *stateData) {
		data.Notify[-100] = notifySettings{Events: []string{eventCompleted}, Thread: 42}
	}); err != nil {
		t.Fatal(err)
	}
	telegramFake.forbidden = map[int64]bool{222: true}
	ctx := context.Background()
	w := &watcher{}
	app.checkEvents(ctx, w, time.Now())

	rtorrentFake.set(func(f *fakeRtorrent) { f.torrents[2].Finished = 5000 })
	app.checkEvents(ctx, w, time.Now())
	topic, private := nextSent(t, telegramFake), nextSent(t, telegramFake)
	if topic.chatID != -100 || topic.threadID != 42 || private.chatID != 333 || private.threadID != 0 {
		t.Fatalf("completion went to %+v and %+v", topic, private)
	}
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("completion also went elsewhere: %q", sent)
	}
	app.state.read(func(data *stateData) {
		if _, ok := data.Notify[222]; ok {
			t.Fatal("a chat that blocked the bot stayed subscribed")
		}
		if _, ok := data.Notify[111]; !ok {
			t.Fatal("another chat was unsubscribed")
		}
	})
}

func TestNotifyScreenTogglesAndRemembersTheTopic(t *testing.T) {
	app, telegramFake, _ := buttonApp(t, nil)
	command(app, "notify")
	screen := nextSent(t, telegramFake)
	if !hasButton(screen.buttons, "⬜ Completed downloads") || !hasButton(screen.buttons, "All on") {
		t.Fatalf("notify screen = %v", buttonTexts(screen.buttons))
	}
	events := func() []string {
		var subscribed []string
		app.state.read(func(data *stateData) { subscribed = data.Notify[111].Events })
		return subscribed
	}

	press(app, master, screen.messageID, "nt:completed")
	if got := events(); !slices.Equal(got, []string{eventCompleted}) || !hasButton(lastEdit(t, telegramFake).buttons, "✅ Completed downloads") {
		t.Fatalf("after toggling completed: %v", got)
	}
	press(app, master, screen.messageID, "nt:on")
	if got := events(); len(got) != len(notifyEvents) {
		t.Fatalf("all on = %v", got)
	}
	press(app, master, screen.messageID, "nt:off")
	if got := events(); got != nil {
		t.Fatalf("all off = %v", got)
	}

	app.handle(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: master}, Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup},
		MessageThreadID: 42, Text: "/notify on",
	}})
	app.state.read(func(data *stateData) {
		if settings := data.Notify[-100]; settings.Thread != 42 || len(settings.Events) != len(notifyEvents) {
			t.Fatalf("group settings = %+v", settings)
		}
	})
	drain(telegramFake)
	if usage := lastSentText(t, telegramFake, app, "notify sometimes"); usage != "notify: use notify, notify on, or notify off" {
		t.Fatalf("usage = %q", usage)
	}
}

// lastSentText runs a command and returns the bot's reply.
func lastSentText(t *testing.T, telegramFake *fakeTelegram, app *application, text string) string {
	t.Helper()
	command(app, text)
	return nextSent(t, telegramFake).text
}

func TestNotificationCardHasNoBack(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	subscribe(t, app, 111, eventCompleted)
	w := &watcher{}
	app.checkEvents(context.Background(), w, time.Now())
	rtorrentFake.set(func(f *fakeRtorrent) { f.torrents[1].Finished = 9000 })
	app.checkEvents(context.Background(), w, time.Now())
	message := nextSent(t, telegramFake)

	press(app, master, message.messageID, "t:"+strings.Repeat("B", 40))
	card := lastEdit(t, telegramFake)
	if !strings.HasPrefix(card.text, "Ubuntu\n") || hasButton(card.buttons, "« Back") || !hasButton(card.buttons, "🔄 Refresh") {
		t.Fatalf("card from a notification = %q %v", card.text, buttonTexts(card.buttons))
	}
}

func TestNotificationFlags(t *testing.T) {
	env := map[string]string{"RT_TOKEN": "123:SECRET", "RT_MASTERS": "7"}
	getenv := func(name string) string { return env[name] }
	for _, args := range [][]string{{"-watch-interval", "1s"}, {"-low-disk", "lots"}, {"-stall-after", "-1m"}} {
		if _, err := parseConfig(args, getenv, io.Discard); err == nil {
			t.Errorf("parseConfig(%v) succeeded", args)
		}
	}
	cfg, err := parseConfig(nil, getenv, io.Discard)
	if err != nil || cfg.watchInterval != defaultWatchInterval || cfg.stallAfter != defaultStallAfter || cfg.lowDisk != 5<<30 {
		t.Fatalf("defaults = %v %v %d, %v", cfg.watchInterval, cfg.stallAfter, cfg.lowDisk, err)
	}
}
