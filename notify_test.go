package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
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
	if sent := drain(telegramFake); len(sent) != 1 || sent[0] != "🐢 Stalled: slow\nNo progress for 31m, at 10.0%" {
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

	// Without downloads, seeding torrents tell where data goes.
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.freeSpace = 1 << 30
		f.torrents = rtapi.Torrents{
			{Name: "stopped", Hash: strings.Repeat("A", 40), State: rtapi.Stopped, Age: 9},
			{Name: "seeding", Hash: strings.Repeat("B", 40), State: rtapi.Seeding, Age: 1},
		}
	})
	seeding := &watcher{}
	app.checkEvents(ctx, seeding, time.Now())
	app.checkEvents(ctx, seeding, time.Now()) // still low: not repeated
	if sent := drain(telegramFake); len(sent) != 1 || !strings.Contains(sent[0], "1.0 GiB free") {
		t.Fatalf("with only seeding torrents = %q", sent)
	}

	// Torrents rTorrent has not opened report no free space, which is not
	// a full disk.
	recovered := &watcher{}
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.freeSpace = 100 << 30
		f.torrents = rtapi.Torrents{
			{Name: "stopped", Hash: strings.Repeat("A", 40), State: rtapi.Stopped, Age: 9},
			{Name: "complete", Hash: strings.Repeat("C", 40), State: rtapi.Complete, Age: 5},
		}
	})
	app.checkEvents(ctx, recovered, time.Now())
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("with no active torrents = %q", sent)
	}

	app.lowDisk = 0
	rtorrentFake.set(func(f *fakeRtorrent) { f.torrents = handlerTorrents() })
	before := len(rtorrentFake.called("d.free_diskspace"))
	app.checkEvents(ctx, w, time.Now())
	if after := len(rtorrentFake.called("d.free_diskspace")); after != before {
		t.Fatal("disk space was checked with -low-disk 0")
	}
}

// The watcher asks rTorrent only for what subscribed chats want: free space
// where downloads land, in one request, and the trackers of new errors.
func TestWatcherAsksOnlyForWhatChatsWant(t *testing.T) {
	var torrents rtapi.Torrents
	for i := range 12 {
		torrents = append(torrents, &rtapi.Torrent{Name: fmt.Sprint("download ", i), Hash: fmt.Sprintf("%040X", i), State: rtapi.Leeching})
	}
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	app.lowDisk = 5 << 30
	// Downloads may land on different disks; the fullest counts.
	rtorrentFake.set(func(f *fakeRtorrent) { f.freeSpace, f.spaceOf = 9<<30, map[string]uint64{torrents[9].Hash: 1 << 30} })
	subscribe(t, app, 111, eventCompleted)
	ctx := context.Background()
	w := &watcher{}
	app.checkEvents(ctx, w, time.Now())
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.torrents[0].State, f.torrents[0].Message = rtapi.Error, "Unregistered torrent"
	})
	app.checkEvents(ctx, w, time.Now())
	if calls := rtorrentFake.called("d.free_diskspace", "t.url"); len(calls) != 0 {
		t.Fatalf("calls for events nobody wants: %v", calls)
	}

	subscribe(t, app, 111, eventDisk)
	app.checkEvents(ctx, w, time.Now())
	if requests, calls := rtorrentFake.requestsContaining("d.free_diskspace"), rtorrentFake.called("d.free_diskspace"); len(requests) != 1 || len(calls) != maxDiskChecks {
		t.Fatalf("free space asked in %d requests with %d calls", len(requests), len(calls))
	}
	if sent := drain(telegramFake); len(sent) != 1 || !strings.Contains(sent[0], "1.0 GiB free") {
		t.Fatalf("low disk = %q", sent)
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

// With no chat subscribed, the watcher does not list torrents; once one
// subscribes, it is told only of what happens from then on.
func TestWatcherIdlesWithoutSubscribersAndStartsAfresh(t *testing.T) {
	seeding := func(name, hash string, finished uint64) *rtapi.Torrent {
		return &rtapi.Torrent{Name: name, Hash: strings.Repeat(hash, 40), State: rtapi.Seeding, Finished: finished}
	}
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{seeding("earlier", "A", 100)})
	ctx := context.Background()
	w := &watcher{}
	subscribe(t, app, 111, eventCompleted, eventErrors)
	app.checkEvents(ctx, w, time.Now())

	subscribe(t, app, 111) // every notification turned off
	listed := len(rtorrentFake.called("d.multicall2"))
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.torrents = append(f.torrents, seeding("meanwhile", "B", 200),
			&rtapi.Torrent{Name: "broken", Hash: strings.Repeat("C", 40), State: rtapi.Error, Message: "Tracker: [Timeout was reached]"})
	})
	app.checkEvents(ctx, w, time.Now())
	app.checkEvents(ctx, w, time.Now())
	if calls := len(rtorrentFake.called("d.multicall2")); calls != listed {
		t.Fatalf("listed torrents %d times with nobody subscribed", calls-listed)
	}

	subscribe(t, app, 111, eventCompleted, eventErrors)
	app.checkEvents(ctx, w, time.Now())
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("announced what happened while nobody was subscribed: %q", sent)
	}
	rtorrentFake.set(func(f *fakeRtorrent) { f.torrents = append(f.torrents, seeding("later", "D", 300)) })
	app.checkEvents(ctx, w, time.Now())
	if sent := drain(telegramFake); len(sent) != 1 || !strings.Contains(sent[0], "Completed: later") {
		t.Fatalf("after subscribing = %q", sent)
	}
}

// A check that finds more than a few events of a kind sends one message
// listing them, rather than one each.
func TestManyEventsComeInOneMessage(t *testing.T) {
	var torrents rtapi.Torrents
	for i := range 12 {
		torrents = append(torrents, &rtapi.Torrent{Name: fmt.Sprintf("download %02d", i), Hash: fmt.Sprintf("%040X", i+1),
			State: rtapi.Leeching, Size: 1 << 20, Completed: 1 << 19, Percent: "50.0%"})
	}
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	app.stallAfter = 30 * time.Minute
	subscribe(t, app, 111, eventCompleted, eventStalled)
	ctx := context.Background()
	w := &watcher{}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	app.checkEvents(ctx, w, start)
	finish := func(from, to int, at uint64) {
		rtorrentFake.set(func(f *fakeRtorrent) {
			for _, torrent := range f.torrents[from:to] {
				torrent.State, torrent.Completed, torrent.Finished = rtapi.Seeding, torrent.Size, at
			}
		})
	}

	finish(0, 3, 1000)
	app.checkEvents(ctx, w, start.Add(time.Minute))
	for i := range 3 {
		if message := nextSent(t, telegramFake); message.text != fmt.Sprintf("✅ Completed: download %02d\n1.0 MiB, ratio 0.00", i) ||
			!hasButton(message.buttons, "ℹ Details") {
			t.Fatalf("completion %d = %q %v", i, message.text, buttonTexts(message.buttons))
		}
	}
	finish(3, 7, 2000)
	app.checkEvents(ctx, w, start.Add(2*time.Minute))
	message := nextSent(t, telegramFake)
	want := "✅ Completed: 4 downloads\n• download 03, 1.0 MiB\n• download 04, 1.0 MiB\n• download 05, 1.0 MiB\n• download 06, 1.0 MiB"
	if message.text != want || len(message.buttons) != 0 {
		t.Fatalf("four completions = %q %v, want %q", message.text, buttonTexts(message.buttons), want)
	}

	app.checkEvents(ctx, w, start.Add(31*time.Minute))
	stalled := drain(telegramFake)
	if len(stalled) != 1 || !strings.HasPrefix(stalled[0], "🐢 Stalled: 5 downloads\n• download 07, at 50.0%, no progress for 31m\n") ||
		!strings.HasSuffix(stalled[0], "\n• download 11, at 50.0%, no progress for 31m") {
		t.Fatalf("five stalls = %q", stalled)
	}
}

func TestTogetherListsUpToTen(t *testing.T) {
	var events []event
	for i := range 12 {
		events = append(events, event{kind: eventCompleted, text: "alone", line: fmt.Sprintf("line %d", i), hash: "H"})
	}
	if got := together(events[:3], "%d of them"); len(got) != 3 || got[0].text != "alone" {
		t.Fatalf("three events = %+v", got)
	}
	got := together(events, "%d of them")
	if len(got) != 1 || got[0].kind != eventCompleted || got[0].hash != "" ||
		!strings.HasPrefix(got[0].text, "12 of them\n• line 0\n") || !strings.HasSuffix(got[0].text, "\n• line 9\n• and 2 more") {
		t.Fatalf("twelve events = %+v", got)
	}
}

// A failing tracker shows torrent by torrent, as each announces. The first
// errors come at once; more with the same tracker and message wait an hour,
// then come in one message unless they have cleared. The wait doubles while
// they keep coming, and starts over once the tracker is calm.
func TestErrorsFromAFailingTrackerAreHeldTogether(t *testing.T) {
	failing, _ := url.Parse("https://failing.example/announce")
	other, _ := url.Parse("https://other.example/announce")
	var torrents rtapi.Torrents
	for i := range 9 {
		torrents = append(torrents, &rtapi.Torrent{Name: fmt.Sprintf("seed %d", i+1), Hash: fmt.Sprintf("%040X", i+1), State: rtapi.Seeding, Tracker: failing})
	}
	torrents = append(torrents, &rtapi.Torrent{Name: "elsewhere", Hash: strings.Repeat("F", 40), State: rtapi.Seeding, Tracker: other})
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	subscribe(t, app, 111, eventErrors)
	ctx := context.Background()
	w := &watcher{}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(minute int) []sentMessage {
		app.checkEvents(ctx, w, start.Add(time.Duration(minute)*time.Minute))
		var sent []sentMessage
		for {
			select {
			case message := <-telegramFake.sent:
				sent = append(sent, message)
			default:
				return sent
			}
		}
	}
	const (
		timeout      = "Tracker: [Timeout was reached]"
		unregistered = `Tracker: [Failure reason "Unregistered torrent"]`
		unresolved   = "Tracker: [Could not resolve host]"
	)
	set := func(state, message string, indexes ...int) {
		rtorrentFake.set(func(f *fakeRtorrent) {
			for _, i := range indexes {
				f.torrents[i].State, f.torrents[i].Message = state, message
			}
		})
	}
	at(0)

	set(rtapi.Error, timeout, 0, 1)
	if sent := at(1); len(sent) != 2 || sent[0].text != "⚠️ Error: seed 1\n"+timeout || !hasButton(sent[1].buttons, "ℹ Details") {
		t.Fatalf("the first errors = %+v", sent)
	}
	if calls := rtorrentFake.called("t.url"); len(calls) != 2 {
		t.Fatalf("fetched %d trackers, want those of the 2 torrents with new errors", len(calls))
	}

	// More of the same wait; another tracker's error does not.
	set(rtapi.Error, timeout, 2, 3, 4, 5, 6, 7, 8)
	set(rtapi.Error, unregistered, 9)
	if sent := at(10); len(sent) != 1 || sent[0].text != "⚠️ Error: elsewhere\n"+unregistered {
		t.Fatalf("while the failing tracker's errors wait = %+v", sent)
	}
	// While they wait, one clears, one is stopped, one's error changes, and one
	// clears and gets another error, which comes at once.
	set(rtapi.Seeding, "", 2, 5)
	set(rtapi.Stopped, timeout, 3)
	set(rtapi.Error, unresolved, 4)
	at(30)
	set(rtapi.Error, unregistered, 5)
	if sent := at(40); len(sent) != 1 || sent[0].text != "⚠️ Error: seed 6\n"+unregistered {
		t.Fatalf("a new error while others wait = %+v", sent)
	}
	if sent := at(60); len(sent) != 0 {
		t.Fatalf("before the hour was up: %+v", sent)
	}
	want := "⚠️ Errors: 4 torrents\n• failing.example, 3 torrents: " + timeout + "\n• failing.example: " + unresolved + " (seed 5)" +
		"\n\n/errors lists every torrent with an error."
	if sent := at(61); len(sent) != 1 || sent[0].text != want || len(sent[0].buttons) != 0 {
		t.Fatalf("after the hour = %+v, want %q", sent, want)
	}

	// While they keep coming, the wait doubles: two hours, then four.
	set(rtapi.Error, timeout, 2)
	for _, minute := range []int{90, 180} {
		if sent := at(minute); len(sent) != 0 {
			t.Fatalf("at minute %d, during the second wait: %+v", minute, sent)
		}
	}
	if sent := at(181); len(sent) != 1 || sent[0].text != "⚠️ Error: seed 3\n"+timeout {
		t.Fatalf("after two hours = %+v", sent)
	}

	// A wait that ends with nothing waiting means the tracker is calm, so the
	// next error comes at once.
	set(rtapi.Seeding, "", 0, 1, 2, 3, 4, 5, 6, 7, 8)
	at(200)
	at(181 + 240)
	set(rtapi.Error, timeout, 0)
	if sent := at(422); len(sent) != 1 || sent[0].text != "⚠️ Error: seed 1\n"+timeout {
		t.Fatalf("once calm = %+v", sent)
	}
	// And the next wait is an hour again.
	set(rtapi.Error, timeout, 1)
	if sent := at(430); len(sent) != 0 {
		t.Fatalf("during the first wait after the calm: %+v", sent)
	}
	if sent := at(482); len(sent) != 1 || sent[0].text != "⚠️ Error: seed 2\n"+timeout {
		t.Fatalf("an hour after the calm = %+v", sent)
	}
}

// An error that changes while it waits comes once, with its new message.
func TestAnErrorThatChangesWhileItWaitsComesOnce(t *testing.T) {
	tracker, _ := url.Parse("https://tracker.example/announce")
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{
		{Name: "first", Hash: strings.Repeat("A", 40), State: rtapi.Seeding, Tracker: tracker},
		{Name: "second", Hash: strings.Repeat("B", 40), State: rtapi.Seeding, Tracker: tracker},
	})
	subscribe(t, app, 111, eventErrors)
	ctx := context.Background()
	w := &watcher{}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(minute int) []string {
		app.checkEvents(ctx, w, start.Add(time.Duration(minute)*time.Minute))
		return drain(telegramFake)
	}
	setError := func(i int, message string) {
		rtorrentFake.set(func(f *fakeRtorrent) { f.torrents[i].State, f.torrents[i].Message = rtapi.Error, message })
	}
	at(0)
	setError(0, "Tracker: [Timeout was reached]")
	at(1)
	setError(1, "Tracker: [Timeout was reached]")
	at(10)
	setError(1, "Tracker: [Could not resolve host]")
	at(20)
	if sent := at(61); len(sent) != 1 || sent[0] != "⚠️ Error: second\nTracker: [Could not resolve host]" {
		t.Fatalf("when the wait is over = %q", sent)
	}
	if sent := at(62); len(sent) != 0 {
		t.Fatalf("came again: %q", sent)
	}
}

// Turning error notifications off forgets the errors held back, so once they
// are on again, the next error comes at once.
func TestErrorHoldsEndWhenNobodyWantsErrors(t *testing.T) {
	tracker, _ := url.Parse("https://tracker.example/announce")
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{
		{Name: "first", Hash: strings.Repeat("A", 40), State: rtapi.Seeding, Tracker: tracker},
		{Name: "second", Hash: strings.Repeat("B", 40), State: rtapi.Seeding, Tracker: tracker},
	})
	subscribe(t, app, 111, eventErrors)
	ctx := context.Background()
	w := &watcher{}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(minute int) []string {
		app.checkEvents(ctx, w, start.Add(time.Duration(minute)*time.Minute))
		return drain(telegramFake)
	}
	setError := func(i int) {
		rtorrentFake.set(func(f *fakeRtorrent) {
			f.torrents[i].State, f.torrents[i].Message = rtapi.Error, "Tracker: [Timeout was reached]"
		})
	}
	at(0)
	setError(0)
	if sent := at(1); len(sent) != 1 {
		t.Fatalf("the first error = %q", sent)
	}
	subscribe(t, app, 111, eventCompleted)
	at(2)
	subscribe(t, app, 111, eventErrors)
	setError(1)
	if sent := at(3); len(sent) != 1 || sent[0] != "⚠️ Error: second\nTracker: [Timeout was reached]" {
		t.Fatalf("after turning errors off and on = %q", sent)
	}
}

// Chats are told when rTorrent has not answered for a minute, when it
// answers again, and when it restarted, which they learn from its process
// ID; a moment without an answer is not an outage.
func TestRTorrentOutagesAndRestartsAreAnnounced(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	subscribe(t, app, 111, eventRtorrent, eventCompleted)
	answering := func(on bool) {
		rtorrentFake.set(func(f *fakeRtorrent) { f.faults = map[string]bool{"system.pid": !on, "d.multicall2": !on} })
	}
	ctx, w := context.Background(), &watcher{}
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	at := func(seconds int) []string {
		app.checkEvents(ctx, w, start.Add(time.Duration(seconds)*time.Second))
		return drain(telegramFake)
	}
	at(0)

	answering(false)
	at(30)
	answering(true)
	if sent := at(60); len(sent) != 0 {
		t.Fatalf("after one unanswered check = %q", sent)
	}

	answering(false)
	if sent := at(90); len(sent) != 0 {
		t.Fatalf("at first = %q", sent)
	}
	if sent := at(150); len(sent) != 1 || !strings.HasPrefix(sent[0], "🔴 rTorrent is not answering: ") {
		t.Fatalf("after a minute = %q", sent)
	}
	// While rTorrent is down, the watcher asks it nothing more.
	lists := len(rtorrentFake.called("d.multicall2"))
	if sent := at(180); len(sent) != 0 || len(rtorrentFake.called("d.multicall2")) != lists {
		t.Fatalf("still down = %q", sent)
	}
	answering(true)
	rtorrentFake.set(func(f *fakeRtorrent) { f.pid = 5678 })
	if sent := at(390); len(sent) != 1 || sent[0] != "🟢 rTorrent is answering again, after 5m. It restarted." {
		t.Fatalf("back = %q", sent)
	}
	rtorrentFake.set(func(f *fakeRtorrent) { f.torrents[1].Finished = 9000 })
	if sent := at(420); len(sent) != 1 || !strings.HasPrefix(sent[0], "✅ Completed: Ubuntu") {
		t.Fatalf("once back, other events = %q", sent)
	}
	rtorrentFake.set(func(f *fakeRtorrent) { f.pid = 9999 })
	if sent := at(450); len(sent) != 1 || sent[0] != "🔄 rTorrent restarted." {
		t.Fatalf("a restart between checks = %q", sent)
	}

	// Back without having restarted.
	answering(false)
	at(480)
	if sent := at(540); len(sent) != 1 || !strings.HasPrefix(sent[0], "🔴 rTorrent is not answering: ") {
		t.Fatalf("down again = %q", sent)
	}
	answering(true)
	if sent := at(600); len(sent) != 1 || sent[0] != "🟢 rTorrent is answering again, after 2m." {
		t.Fatalf("back without a restart = %q", sent)
	}

	// Chats that do not want to know are not told, and the watcher does not
	// ask rTorrent for its process ID for them.
	subscribe(t, app, 111, eventCompleted)
	before := len(rtorrentFake.called("system.pid"))
	rtorrentFake.set(func(f *fakeRtorrent) { f.pid = 1 })
	if sent := at(630); len(sent) != 0 || len(rtorrentFake.called("system.pid")) != before {
		t.Fatalf("without the subscription = %q", sent)
	}
}

func TestFormatMinutes(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0m", 29 * time.Second: "0m", 31 * time.Second: "1m", 59 * time.Minute: "59m",
		time.Hour: "1h", 90 * time.Minute: "1h30m", 25*time.Hour + time.Minute: "25h1m",
	} {
		if got := formatMinutes(d); got != want {
			t.Errorf("formatMinutes(%s) = %q, want %q", d, got, want)
		}
	}
}
