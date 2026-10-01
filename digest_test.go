package main

import (
	"context"
	"fmt"
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

	if got := lastSentText(t, telegramFake, app, "digest 08:00"); !strings.HasPrefix(got, "The daily digest will come at 08:00.") {
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
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "📰 Daily digest, Fri 2 Oct") ||
		!strings.Contains(sent[0], "Uploaded 1.0 GiB and downloaded 2.0 GiB since the last digest.") {
		t.Fatalf("day 2 = %q", sent)
	}

	// rTorrent restarted, so its counters went back down.
	rtorrentFake.set(func(f *fakeRtorrent) { f.totals = [2]uint64{1 << 30, 1 << 30} })
	app.checkDigest(ctx, day(3, 8, 0))
	if sent := drain(telegramFake); len(sent) != 1 || !strings.Contains(sent[0], "Uploaded 1.0 GiB and downloaded 1.0 GiB since rTorrent started.") {
		t.Fatalf("after an rTorrent restart = %q", sent)
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
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	app.now = func() time.Time { return now }
	rtorrentFake.set(func(f *fakeRtorrent) { f.downRate, f.upRate, f.freeSpace = 2048, 1024, 10<<30 })

	got := lastSentText(t, telegramFake, app, "digest now")
	for _, want := range []string{
		"📰 Daily digest, Mon 5 Oct\n",
		"\nCompleted (12): done 00, done 01, done 02, done 03, done 04, done 05, done 06, done 07, done 08, done 09, and 2 more",
		"\nAdded (1): fresh",
		"since rTorrent started.",
		"\nTorrents: 1 downloading, 13 seeding, 1 with errors.",
		"\nNow: ↓ 2.0 KiB/s ↑ 1.0 KiB/s",
		"\nFree space: 10.0 GiB",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old") {
		t.Errorf("a torrent finished two days ago is in the digest:\n%s", got)
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
		"digest":       "No daily digest in this chat. Set one with digest HH:MM, such as digest 08:00.",
		"digest 25:00": "digest: use digest HH:MM, digest now, or digest off",
		"digest a b":   "digest: use digest HH:MM, digest now, or digest off",
	} {
		if got := lastSentText(t, telegramFake, app, text); got != want {
			t.Errorf("%s = %q, want %q", text, got, want)
		}
	}
	lastSentText(t, telegramFake, app, "digest 07:30")
	if got := lastSentText(t, telegramFake, app, "digest"); !strings.HasPrefix(got, "The daily digest comes at 07:30.") {
		t.Fatalf("digest = %q", got)
	}
	if got := lastSentText(t, telegramFake, app, "digest off"); got != "The daily digest is off." {
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
