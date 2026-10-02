package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

func day(n, hour, minute int) time.Time {
	return time.Date(2026, 10, n, hour, minute, 0, 0, time.Local)
}

func unix(at time.Time) uint64 { return uint64(at.Unix()) }

func TestDigestComesOnceADayAtItsTime(t *testing.T) {
	clock := day(1, 10, 0)
	app, telegramFake, rtorrentFake := buttonApp(t, nil)
	app.now = func() time.Time { return clock }
	ctx := context.Background()

	if got := lastSentText(t, telegramFake, app, "digest 08:00"); !strings.HasPrefix(got, "The daily digest will come every day at 08:00.") {
		t.Fatalf("digest 08:00 = %q", got)
	}
	for _, at := range []time.Time{day(1, 12, 0), day(2, 7, 59)} {
		app.checkDigest(ctx, at)
	}
	if sent := drain(telegramFake); len(sent) != 0 {
		t.Fatalf("a digest came early: %q", sent)
	}

	rtorrentFake.set(func(f *fakeRtorrent) { f.totals = [2]uint64{4 << 30, 7 << 30} })
	app.checkDigest(ctx, day(2, 8, 0))
	app.checkDigest(ctx, day(2, 8, 30))
	sent := drain(telegramFake)
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "📰 Daily digest, Fri 2 Oct\nSince Thu 1 Oct 10:00\n") ||
		!strings.Contains(sent[0], "\nUploaded: 1.0 GiB\nDownloaded: 2.0 GiB\n") {
		t.Fatalf("day 2 = %q", sent)
	}

	// The count goes on while rTorrent restarts, which resets its totals:
	// 2 GiB each way before the restart, then 1 GiB each way after it.
	rtorrentFake.set(func(f *fakeRtorrent) { f.totals = [2]uint64{6 << 30, 9 << 30} })
	app.checkDigest(ctx, day(2, 12, 0))
	rtorrentFake.set(func(f *fakeRtorrent) { f.totals = [2]uint64{1 << 30, 1 << 30} })
	app.checkDigest(ctx, day(3, 8, 0))
	if sent := drain(telegramFake); len(sent) != 1 || !strings.Contains(sent[0], "Since Fri 2 Oct 08:00\n") ||
		!strings.Contains(sent[0], "\nUploaded: 3.0 GiB\nDownloaded: 3.0 GiB\n") {
		t.Fatalf("after an rTorrent restart = %q", sent)
	}
}

// A digest covers the time since the last one: what finished or was added
// before it was sent belongs to it.
func TestDigestCoversTheTimeSinceTheLastOne(t *testing.T) {
	torrent := func(name string, finished, added time.Time) *rtapi.Torrent {
		return &rtapi.Torrent{Name: name, Hash: fmt.Sprintf("%040X", len(name)), State: rtapi.Seeding, Finished: unix(finished), Age: unix(added)}
	}
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{
		torrent("before setup", day(1, 9, 0), day(1, 9, 0)),
		torrent("after setup", day(1, 11, 0), day(1, 10, 30)),
	})
	app.now = func() time.Time { return day(1, 10, 0) }
	lastSentText(t, telegramFake, app, "digest 08:00")
	app.checkDigest(context.Background(), day(2, 8, 0))
	if sent := drain(telegramFake); len(sent) != 1 || !strings.Contains(sent[0], "\nCompleted: 1\nAdded: 1\n") {
		t.Fatalf("first digest = %q", sent)
	}
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.torrents = append(f.torrents, torrent("later", day(2, 9, 0), day(2, 7, 59)))
	})
	app.checkDigest(context.Background(), day(3, 8, 0))
	if sent := drain(telegramFake); len(sent) != 1 || !strings.Contains(sent[0], "\nCompleted: 1\nAdded: 0\n") {
		t.Fatalf("second digest = %q", sent)
	}
}

func TestDigestMissedWhileOfflineComesOnce(t *testing.T) {
	clock := day(1, 9, 0)
	app, telegramFake, _ := buttonApp(t, nil)
	app.now = func() time.Time { return clock }
	lastSentText(t, telegramFake, app, "digest 08:00")
	app.checkDigest(context.Background(), day(2, 20, 0))
	app.checkDigest(context.Background(), day(2, 20, 1))
	if sent := drain(telegramFake); len(sent) != 1 {
		t.Fatalf("missed digest = %q", sent)
	}
}

func TestDigestSummarizesTheDay(t *testing.T) {
	now := day(5, 8, 0)
	var torrents rtapi.Torrents
	for i := range 12 {
		torrents = append(torrents, &rtapi.Torrent{
			Name: fmt.Sprintf("done %02d", i), Hash: fmt.Sprintf("%040X", i), State: rtapi.Seeding,
			Finished: unix(now.Add(-time.Duration(12-i) * time.Hour)), Age: unix(now.Add(-72 * time.Hour)),
		})
	}
	torrents = append(torrents,
		&rtapi.Torrent{Name: "old", Hash: strings.Repeat("A", 40), State: rtapi.Seeding, Finished: unix(now.Add(-48 * time.Hour)), Age: unix(now.Add(-96 * time.Hour))},
		&rtapi.Torrent{Name: "fresh", Hash: strings.Repeat("B", 40), State: rtapi.Leeching, Age: unix(now.Add(-time.Hour))},
		&rtapi.Torrent{Name: "broken", Hash: strings.Repeat("C", 40), State: rtapi.Error, Message: "failed", Age: unix(now.Add(-96 * time.Hour))},
	)
	// Errors with the same tracker and message are counted together.
	gone, _ := url.Parse("https://gone.example/announce")
	slow, _ := url.Parse("udp://slow.example:80")
	for i, problem := range []struct {
		tracker *url.URL
		message string
	}{
		{gone, `Tracker: [Failure reason "Unregistered torrent"]`},
		{gone, `Tracker: [Failure reason "Unregistered torrent"]`},
		{gone, `Tracker: [Failure reason "Unregistered torrent"]`},
		{slow, "Tracker: [Timeout was reached]"},
		{slow, "Tracker: [Timeout was reached]"},
		{gone, "Tracker: [Timeout was reached]"},
	} {
		torrents = append(torrents, &rtapi.Torrent{Name: fmt.Sprintf("error %d", i), Hash: fmt.Sprintf("E%039X", i),
			State: rtapi.Error, Message: problem.message, Tracker: problem.tracker, Age: unix(now.Add(-96 * time.Hour))})
	}
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	app.now = func() time.Time { return now }
	rtorrentFake.set(func(f *fakeRtorrent) { f.downRate, f.upRate, f.freeSpace = 2048, 1024, 10<<30 })

	got := lastSentText(t, telegramFake, app, "digest now")
	for _, want := range []string{
		"📰 Daily digest, Mon 5 Oct\n",
		"\nCompleted: 12\nAdded: 1\n",
		"\nUploaded: 0 B\nDownloaded: 0 B\n(counted since Mon 5 Oct 08:00)",
		"\n\nTorrents: 1 downloading, 13 seeding, 7 with errors\nErrors:" +
			"\n• gone.example, 3 torrents: Tracker: [Failure reason \"Unregistered torrent\"]" +
			"\n• slow.example, 2 torrents: Tracker: [Timeout was reached]" +
			"\n• gone.example: Tracker: [Timeout was reached] (error 5)" +
			"\n• unknown: failed (broken)" +
			"\n\nFree space: 10.0 GiB",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "↓") {
		t.Errorf("the digest shows current speeds:\n%s", got)
	}
	if strings.Contains(got, "done 00") {
		t.Errorf("the digest names torrents:\n%s", got)
	}
	app.state.read(func(data *stateData) {
		if len(data.Digest) != 0 {
			t.Fatal("digest now scheduled a digest")
		}
	})
}

func TestDigestCommandsTopicsAndBlockedChats(t *testing.T) {
	clock := day(1, 6, 0)
	app, telegramFake, _ := buttonApp(t, nil)
	app.now = func() time.Time { return clock }
	for text, want := range map[string]string{
		"digest":                   "No digest in this chat. Set one with digest HH:MM [daily|weekly|monthly], such as digest 08:00.",
		"digest 25:00":             "digest: use digest HH:MM [daily|weekly|monthly], digest now, or digest off",
		"digest a b":               "digest: use digest HH:MM [daily|weekly|monthly], digest now, or digest off",
		"digest 08:00 fortnightly": "digest: use digest HH:MM [daily|weekly|monthly], digest now, or digest off",
		"digest now please":        "digest: use digest HH:MM [daily|weekly|monthly], digest now, or digest off",
		"digest 08:00 weekly x":    "digest: use digest HH:MM [daily|weekly|monthly], digest now, or digest off",
	} {
		if got := lastSentText(t, telegramFake, app, text); got != want {
			t.Errorf("%s = %q, want %q", text, got, want)
		}
	}
	lastSentText(t, telegramFake, app, "digest 07:30 Daily")
	if got := lastSentText(t, telegramFake, app, "digest"); !strings.HasPrefix(got, "The daily digest comes every day at 07:30.") {
		t.Fatalf("digest = %q", got)
	}
	app.state.read(func(data *stateData) {
		if every := data.Digest[111].Every; every != "" {
			t.Errorf("daily was saved as %q", every)
		}
	})
	if got := lastSentText(t, telegramFake, app, "digest off"); got != "The digest is off." {
		t.Fatalf("digest off = %q", got)
	}

	app.handle(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: master}, Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup},
		MessageThreadID: 42, Text: "/digest 07:30",
	}})
	nextSent(t, telegramFake)
	telegramFake.forbidden = map[int64]bool{111: true}
	command(app, "digest 07:30") // fails to send, but is saved
	app.checkDigest(context.Background(), day(1, 7, 30))
	topic := nextSent(t, telegramFake)
	if topic.chatID != -100 || topic.threadID != 42 || !strings.HasPrefix(topic.text, "📰") {
		t.Fatalf("group digest = %+v", topic)
	}
	app.state.read(func(data *stateData) {
		if _, ok := data.Digest[111]; ok {
			t.Fatal("a chat that blocked the bot kept its digest")
		}
	})
}

// rTorrent reports no free space for torrents it has not opened, so a
// digest with none active leaves free space out rather than say 0 B.
func TestDigestOmitsFreeSpaceWithoutActiveTorrents(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{
		{Name: "stopped", Hash: strings.Repeat("A", 40), State: rtapi.Stopped},
		{Name: "complete", Hash: strings.Repeat("B", 40), State: rtapi.Complete, Age: 7},
	})
	rtorrentFake.set(func(f *fakeRtorrent) { f.freeSpace = 10 << 30 })
	if got := lastSentText(t, telegramFake, app, "digest now"); strings.Contains(got, "Free space") {
		t.Fatalf("digest without active torrents:\n%s", got)
	}
}

// A chat that sets up its digest while another chat's is counting gets only
// the traffic since its own setup.
func TestDigestCountsTrafficFromEachChatsSetup(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, nil)
	ctx := context.Background()
	app.now = func() time.Time { return day(1, 10, 0) }
	app.digest(ctx, 111, []string{"08:00"})
	rtorrentFake.set(func(f *fakeRtorrent) { f.totals = [2]uint64{4 << 30, 5 << 30} })
	app.checkDigest(ctx, day(1, 12, 0))
	app.now = func() time.Time { return day(1, 13, 0) }
	app.digest(ctx, 222, []string{"08:00"})
	drain(telegramFake)

	rtorrentFake.set(func(f *fakeRtorrent) { f.totals = [2]uint64{6 << 30, 5 << 30} })
	app.checkDigest(ctx, day(2, 8, 0))
	uploaded := make(map[int64]string)
	for range 2 {
		message := nextSent(t, telegramFake)
		uploaded[message.chatID] = message.text
	}
	if !strings.Contains(uploaded[111], "\nUploaded: 3.0 GiB\n") || !strings.Contains(uploaded[222], "\nUploaded: 2.0 GiB\n") {
		t.Fatalf("digests = %v", uploaded)
	}
}

func TestDigestLastDue(t *testing.T) {
	at := func(year int, month time.Month, day, hour, minute int) time.Time {
		return time.Date(year, month, day, hour, minute, 0, 0, time.Local)
	}
	for _, test := range []struct {
		every string
		now   time.Time
		want  time.Time
	}{
		{"", at(2026, 10, 1, 7, 59), at(2026, 9, 30, 8, 0)},
		{"", at(2026, 10, 1, 8, 0), at(2026, 10, 1, 8, 0)},
		{"weekly", at(2026, 10, 1, 10, 0), at(2026, 9, 28, 8, 0)}, // a Thursday
		{"weekly", at(2026, 10, 5, 7, 59), at(2026, 9, 28, 8, 0)}, // a Monday, before the time
		{"weekly", at(2026, 10, 5, 8, 0), at(2026, 10, 5, 8, 0)},
		{"weekly", at(2026, 10, 4, 23, 0), at(2026, 9, 28, 8, 0)}, // a Sunday
		{"monthly", at(2026, 10, 1, 7, 59), at(2026, 9, 1, 8, 0)},
		{"monthly", at(2026, 10, 31, 23, 0), at(2026, 10, 1, 8, 0)},
		{"monthly", at(2027, 1, 1, 7, 0), at(2026, 12, 1, 8, 0)},
	} {
		settings := digestSettings{Time: "08:00", Every: test.every}
		if got := settings.lastDue(test.now); !got.Equal(test.want) {
			t.Errorf("%s digest at %s: last due %s, want %s", settings.period(), test.now, got, test.want)
		}
	}
}

func TestWeeklyAndMonthlyDigests(t *testing.T) {
	app, telegramFake, _ := buttonApp(t, nil)
	ctx := context.Background()
	app.now = func() time.Time { return day(1, 10, 0) } // a Thursday
	if got := lastSentText(t, telegramFake, app, "digest 08:00 weekly"); !strings.HasPrefix(got, "The weekly digest will come on Mondays at 08:00.") {
		t.Fatalf("digest 08:00 weekly = %q", got)
	}
	app.digest(ctx, 222, []string{"08:00", "monthly"})
	if got := nextSent(t, telegramFake).text; !strings.HasPrefix(got, "The monthly digest will come on the 1st of each month at 08:00.") {
		t.Fatalf("digest 08:00 monthly = %q", got)
	}

	sent := func(at time.Time) []string {
		app.checkDigest(ctx, at)
		return drain(telegramFake)
	}
	for _, at := range []time.Time{day(1, 12, 0), day(4, 23, 0), day(5, 7, 59)} {
		if got := sent(at); len(got) != 0 {
			t.Fatalf("at %s: %q", at, got)
		}
	}
	if got := sent(day(5, 8, 0)); len(got) != 1 || !strings.HasPrefix(got[0], "📰 Weekly digest, Mon 5 Oct\nSince Thu 1 Oct 10:00\n") {
		t.Fatalf("first weekly digest = %q", got)
	}
	// Offline from Monday to Tuesday: one digest when back, then the next Monday.
	if got := sent(day(13, 20, 0)); len(got) != 1 || !strings.Contains(got[0], "Since Mon 5 Oct 08:00\n") {
		t.Fatalf("missed weekly digest = %q", got)
	}
	// Nor does a clock set back, as by a time sync, send it again.
	for _, at := range []time.Time{day(13, 21, 0), day(11, 9, 0), day(18, 8, 0)} {
		if got := sent(at); len(got) != 0 {
			t.Fatalf("at %s: %q", at, got)
		}
	}
	if got := sent(day(19, 8, 0)); len(got) != 1 {
		t.Fatalf("third weekly digest = %q", got)
	}

	if got := sent(day(26, 8, 0)); len(got) != 1 || !strings.HasPrefix(got[0], "📰 Weekly digest") {
		t.Fatalf("fourth weekly digest = %q", got)
	}
	november := time.Date(2026, 11, 1, 8, 0, 0, 0, time.Local) // a Sunday
	if got := sent(november.Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("before the 1st: %q", got)
	}
	got := sent(november)
	if len(got) != 1 || !strings.HasPrefix(got[0], "📰 Monthly digest, Sun 1 Nov\nSince Thu 1 Oct 10:00\n") {
		t.Fatalf("monthly digest = %q", got)
	}
}
