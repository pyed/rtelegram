package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/pyed/rtapi"
)

// Fetching trackers takes a call per torrent, so only the replies that show
// them may ask for them.
func TestOnlyTrackerRepliesFetchTrackers(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, handlerTorrents())
	for _, text := range []string{"list", "down", "seeding", "paused", "checking", "errors", "active", "head", "tail",
		"search deb", "latest", "count", "speed", "sort name"} {
		command(app, text)
		drain(telegramFake)
	}
	app.checkEvents(context.Background(), &watcher{}, time.Now())
	app.checkDigest(context.Background(), time.Now())
	app.wg.Wait()
	if calls := rtorrentFake.called("t.url"); len(calls) != 0 {
		t.Fatalf("lists without trackers fetched %d trackers", len(calls))
	}

	// A digest names the trackers of torrents with errors, and only those.
	command(app, "digest now")
	drain(telegramFake)
	if calls := rtorrentFake.called("t.url"); len(calls) != 1 || calls[0][0] != strings.Repeat("E", 40)+":t0" {
		t.Fatalf("digest fetched trackers %v, want only the errored torrent's", calls)
	}

	command(app, "trackers")
	if text := nextSent(t, telegramFake).text; !strings.Contains(text, "2 - tracker.example.org") {
		t.Fatalf("trackers = %q", text)
	}
	if calls := rtorrentFake.called("t.url"); len(calls) != 1+len(handlerTorrents()) {
		t.Fatalf("trackers fetched %d trackers, want one per torrent", len(calls))
	}

	command(app, "list other.net")
	if text := nextSent(t, telegramFake).text; !strings.Contains(text, "Ubuntu") || strings.Contains(text, "Debian") {
		t.Fatalf("list other.net = %q", text)
	}

	before := len(rtorrentFake.called("t.url"))
	command(app, "info bbbbbbb")
	if text := nextSent(t, telegramFake).text; !strings.Contains(text, "tracker.other.net") {
		t.Fatalf("info card = %q, want its tracker", text)
	}
	if fetched := len(rtorrentFake.called("t.url")) - before; fetched != 1 {
		t.Fatalf("info fetched %d trackers, want only its torrent's", fetched)
	}
}

// Confirming an add polls rTorrent every second, so it lists only hashes and
// then fetches the one new torrent.
func TestAddConfirmationListsOnlyHashes(t *testing.T) {
	library := make(rtapi.Torrents, 200)
	for i := range library {
		library[i] = &rtapi.Torrent{Name: fmt.Sprintf("torrent %d", i), Hash: fmt.Sprintf("%040X", i+1), Age: uint64(i)}
	}
	for _, source := range []string{
		"magnet:?xt=urn:btih:" + strings.Repeat("F", 40) + "&dn=New",
		"https://tracker.example/new.torrent",
	} {
		rtorrentFake, client := newFakeRtorrent(t, library...)
		added := &rtapi.Torrent{Name: "New", Hash: strings.Repeat("F", 40), Age: 1 << 40}
		telegramFake := &fakeTelegram{sent: make(chan sentMessage, 4)}
		app := &application{
			bot: newTestBot(t, telegramFake, "123:SECRET"), rtorrent: client,
			logger: log.New(io.Discard, "", 0), addTimeout: 5 * time.Second, addPollInterval: 10 * time.Millisecond,
		}
		// The torrent appears on the third look.
		go func() {
			for rtorrentFake.countCalls("d.multicall2") < 3 {
				time.Sleep(time.Millisecond)
			}
			rtorrentFake.set(func(f *fakeRtorrent) { f.torrents = append(f.torrents, added) })
		}()
		app.add(context.Background(), 111, []string{source})
		if text := nextSent(t, telegramFake).text; text != "Added: <fffffff> New" {
			t.Fatalf("%s: reply = %q", source, text)
		}
		app.wg.Wait()
		polls := 0
		for _, call := range rtorrentFake.called("d.multicall2") {
			polls++
			if fields := strings.Join(call[2:], " "); fields != "d.hash=" {
				t.Fatalf("%s: listed %s, want only hashes", source, fields)
			}
		}
		if polls < 3 {
			t.Fatalf("%s: %d hash lists; the test expects the torrent to appear on the third", source, polls)
		}
		if names := rtorrentFake.called("d.name"); len(names) != 1 || names[0][0] != added.Hash {
			t.Fatalf("%s: fetched details of %v, want only the new torrent", source, names)
		}
	}
}

// A link's hash is not known in advance, so the newest torrent that appeared
// is the one reported.
func TestAddConfirmationReportsTheNewestOfSeveral(t *testing.T) {
	rtorrentFake, client := newFakeRtorrent(t, &rtapi.Torrent{Name: "old", Hash: strings.Repeat("A", 40), Age: 1})
	rtorrentFake.onLoad(func(string) *rtapi.Torrent {
		rtorrentFake.torrents = append(rtorrentFake.torrents, &rtapi.Torrent{Name: "other", Hash: strings.Repeat("C", 40), Age: 50})
		return &rtapi.Torrent{Name: "linked", Hash: strings.Repeat("B", 40), Age: 100}
	})
	telegramFake := &fakeTelegram{sent: make(chan sentMessage, 4)}
	app := &application{
		bot: newTestBot(t, telegramFake, "123:SECRET"), rtorrent: client,
		logger: log.New(io.Discard, "", 0), addTimeout: time.Second, addPollInterval: 10 * time.Millisecond,
	}
	app.add(context.Background(), 111, []string{"https://tracker.example/linked.torrent"})
	if text := nextSent(t, telegramFake).text; text != "Added: <bbbbbbb> linked" {
		t.Fatalf("reply = %q", text)
	}
	app.wg.Wait()
}
