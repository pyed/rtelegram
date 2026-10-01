package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

// handlerTorrents is a library with one torrent in each state rTorrent
// reports. Their hash prefixes are aaaaaaa through eeeeeee.
func handlerTorrents() rtapi.Torrents {
	example, _ := url.Parse("udp://tracker.example.org:6969/announce")
	other, _ := url.Parse("https://tracker.other.net/announce")
	hash := func(c string) string { return strings.Repeat(c, 40) }
	return rtapi.Torrents{
		{Name: "Debian", Hash: hash("A"), State: rtapi.Leeching, DownRate: 2048, Size: 4096, Completed: 1024, UpTotal: 512, Age: 1000, Tracker: example},
		{Name: "Ubuntu", Hash: hash("B"), State: rtapi.Seeding, UpRate: 1024, Size: 2048, Completed: 2048, UpTotal: 3072, Ratio: 1.5, Age: 3000, Tracker: other},
		{Name: "Arch", Hash: hash("C"), State: rtapi.Stopped, Size: 1024, Completed: 512, Age: 2000, Tracker: example},
		{Name: "Fedora", Hash: hash("D"), State: rtapi.Hashing, Size: 100, Age: 500},
		{Name: "Gentoo", Hash: hash("E"), State: rtapi.Error, Message: "Tracker: [Timeout was reached]", Age: 400},
	}
}

// reply describes an expected bot message: exactly is, or containing every
// string in has and none in lacks.
type reply struct {
	is    string
	has   []string
	lacks []string
}

func TestCommandHandlers(t *testing.T) {
	calledWith := func(method string, hashes ...string) func(*testing.T, *fakeRtorrent) {
		return func(t *testing.T, f *fakeRtorrent) {
			t.Helper()
			calls := f.called(method)
			if len(calls) != len(hashes) {
				t.Fatalf("%s called %d times, want %d", method, len(calls), len(hashes))
			}
			for i, hash := range hashes {
				if calls[i][0] != strings.Repeat(hash, 40) {
					t.Fatalf("%s call %d targeted %s", method, i, calls[i][0])
				}
			}
		}
	}
	tests := []struct {
		name     string
		commands []string
		replies  []reply
		check    func(*testing.T, *fakeRtorrent)
	}{
		{"list", []string{"list"}, []reply{{is: "<aaaaaaa> Debian\n<bbbbbbb> Ubuntu\n<ccccccc> Arch\n<ddddddd> Fedora\n<eeeeeee> Gentoo\n"}}, nil},
		{"list by tracker", []string{"li other"}, []reply{{is: "<bbbbbbb> Ubuntu\n"}}, nil},
		{"list without matches", []string{"list nowhere"}, []reply{{is: "list: No torrents"}}, nil},
		{"down", []string{"dl"}, []reply{{is: "<aaaaaaa> Debian\n"}}, nil},
		{"seeding", []string{"seeding"}, []reply{{is: "<bbbbbbb> Ubuntu\n"}}, nil},
		{"paused", []string{"pa"}, []reply{{has: []string{"<ccccccc> Arch\nStopped 512 B (50.0%)"}, lacks: []string{"Debian"}}}, nil},
		{"checking", []string{"checking"}, []reply{{is: "<ddddddd> Fedora\n"}}, nil},
		{"errors", []string{"er"}, []reply{{is: "<eeeeeee> Gentoo\nTracker: [Timeout was reached]\n\n"}}, nil},
		{"search", []string{"se ub"}, []reply{{is: "<bbbbbbb> Ubuntu\n"}}, nil},
		{"search usage", []string{"search"}, []reply{{is: "search: needs an argument"}}, nil},
		{"search without matches", []string{"search zz"}, []reply{{is: "No matches"}}, nil},
		{"latest", []string{"la 2"}, []reply{{is: "<bbbbbbb> Ubuntu\n<ccccccc> Arch\n"}}, nil},
		{"latest rejects words", []string{"latest two"}, []reply{{is: "latest: argument must be a number"}}, nil},
		{"trackers", []string{"tr"}, []reply{{is: "2 - tracker.example.org\n1 - tracker.other.net\n2 - unknown\n"}}, nil},
		{"count", []string{"co"}, []reply{{is: "Leeching: 1\nSeeding: 1\nComplete: 0\nStopped: 1\nHashing: 1\nError: 1\n\nTotal: 5"}}, nil},
		{"head", []string{"he 2"}, []reply{{has: []string{"<aaaaaaa> Debian\nLeeching", "<bbbbbbb> Ubuntu\nSeeding"}, lacks: []string{"Arch"}}}, nil},
		{"tail", []string{"ta 1"}, []reply{{has: []string{"<eeeeeee> Gentoo\nError"}, lacks: []string{"Fedora"}}}, nil},
		{"active", []string{"ac"}, []reply{{has: []string{"Debian", "Ubuntu"}, lacks: []string{"Arch", "Fedora", "Gentoo"}}}, nil},
		{"sort then list", []string{"so rev name", "list"}, []reply{
			{is: "sort: by reversed name"},
			{is: "<bbbbbbb> Ubuntu\n<eeeeeee> Gentoo\n<ddddddd> Fedora\n<aaaaaaa> Debian\n<ccccccc> Arch\n"},
		}, nil},
		{"sort usage", []string{"sort", "sort bogus"}, []reply{
			{is: "sort: [rev] name|downrate|uprate|size|ratio|age|upload"},
			{is: "sort: unknown sorting method"},
		}, nil},
		{"info", []string{"in bbbbbbb"}, []reply{{has: []string{"Ubuntu\nSeeding 2.0 KiB (100%)", "R: 1.50 UP: 3.0 KiB", "Tracker: tracker.other.net"}}}, nil},
		{"info needs a long prefix", []string{"info bb"}, []reply{{is: "info: torrent hash prefix must contain at least 7 characters"}}, nil},
		{"stop", []string{"sp aaaaaaa"}, []reply{{is: "Stopped: Debian"}}, calledWith("d.stop", "A")},
		{"start all", []string{"st all"}, []reply{{is: "Started: Debian, Ubuntu, Arch, Fedora, Gentoo"}},
			calledWith("d.start", "A", "B", "C", "D", "E")},
		{"check", []string{"ck ccccccc"}, []reply{{is: "Checking: Arch"}}, calledWith("d.check_hash", "C")},
		{"check unknown", []string{"check fffffff"}, []reply{{is: `check: no torrent matches hash prefix "fffffff"`}}, calledWith("d.check_hash")},
		{"del", []string{"del eeeeeee", "list"}, []reply{
			{is: "Deleted: Gentoo"},
			{has: []string{"Debian"}, lacks: []string{"Gentoo"}},
		}, calledWith("d.erase", "E")},
		{"del refuses all", []string{"del all"}, []reply{{is: "del: torrent hash prefix must contain at least 7 characters"}}, calledWith("d.erase")},
		{"stats", []string{"sa"}, []reply{{is: "Throttle: off / off\nPort: 6890\nDirectory: /downloads\n" +
			"Session uploaded: 3.0 GiB\nSession downloaded: 5.0 GiB\n" +
			"Loaded torrents uploaded: 3.5 KiB\nLoaded torrents downloaded: 3.5 KiB\nLoaded torrents ratio: 1.00"}}, nil},
		{"speed", []string{"ss"}, []reply{{is: "↓ 2.0 KiB ↑ 1.0 KiB"}}, nil},
		{"version", []string{"version"}, []reply{{is: "rTorrent/libtorrent: 0.9.8/0.13.8\nrtelegram: " + version}}, nil},
		{"help", []string{"help"}, []reply{{is: helpText}}, nil},
		{"unknown", []string{"bogus"}, []reply{{is: "no such command, try /help"}}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rtorrentFake, client := newFakeRtorrent(t, handlerTorrents()...)
			rtorrentFake.set(func(f *fakeRtorrent) { f.downRate, f.upRate = 2048, 1024 })
			telegramFake := &fakeTelegram{sent: make(chan sentMessage, 8)}
			app := &application{
				bot: newTestBot(t, telegramFake, "123:SECRET"), rtorrent: client,
				logger: log.New(io.Discard, "", 0), token: "123:SECRET",
				masters: principals{ids: map[int64]struct{}{7: {}}}, botUsername: "ThisBot",
				noLive: true, sorts: make(map[int64]sortPreference),
			}
			for _, command := range test.commands {
				app.handle(context.Background(), &models.Update{Message: &models.Message{
					From: &models.User{ID: 7}, Chat: models.Chat{ID: 111, Type: models.ChatTypePrivate}, Text: command,
				}})
			}
			app.wg.Wait()
			for i, want := range test.replies {
				var got sentMessage
				select {
				case got = <-telegramFake.sent:
				case <-time.After(2 * time.Second):
					t.Fatalf("reply %d never arrived", i)
				}
				if got.chatID != 111 {
					t.Fatalf("reply %d went to chat %d", i, got.chatID)
				}
				if want.is != "" && got.text != want.is {
					t.Fatalf("reply %d = %q, want %q", i, got.text, want.is)
				}
				for _, part := range want.has {
					if !strings.Contains(got.text, part) {
						t.Fatalf("reply %d = %q, want it to contain %q", i, got.text, part)
					}
				}
				for _, part := range want.lacks {
					if strings.Contains(got.text, part) {
						t.Fatalf("reply %d = %q, want it without %q", i, got.text, part)
					}
				}
			}
			select {
			case extra := <-telegramFake.sent:
				t.Fatalf("unexpected extra reply %q", extra.text)
			default:
			}
			if test.check != nil {
				test.check(t, rtorrentFake)
			}
		})
	}
}

func TestCountReportsEachState(t *testing.T) {
	// A different number of torrents in each state, so swapped counts show.
	var torrents rtapi.Torrents
	for n, state := range []string{rtapi.Leeching, rtapi.Seeding, rtapi.Complete, rtapi.Stopped, rtapi.Hashing, rtapi.Error} {
		for range n + 1 {
			torrent := &rtapi.Torrent{Name: state, Hash: fmt.Sprintf("%040X", len(torrents)), State: state}
			if state == rtapi.Error {
				torrent.Message = "failed"
			}
			torrents = append(torrents, torrent)
		}
	}
	_, client := newFakeRtorrent(t, torrents...)
	telegramFake := &fakeTelegram{sent: make(chan sentMessage, 1)}
	app := &application{
		bot: newTestBot(t, telegramFake, "123:SECRET"), rtorrent: client,
		logger: log.New(io.Discard, "", 0), token: "123:SECRET",
	}
	app.count(context.Background(), 111)
	expectSent(t, telegramFake.sent, 111, "Leeching: 1\nSeeding: 2\nComplete: 3\nStopped: 4\nHashing: 5\nError: 6\n\nTotal: 21")
}

func TestLiveUpdatesEditTheReply(t *testing.T) {
	rtorrentFake, client := newFakeRtorrent(t, handlerTorrents()...)
	rtorrentFake.set(func(f *fakeRtorrent) { f.downRate, f.upRate = 2048, 1024 })
	telegramFake := &fakeTelegram{sent: make(chan sentMessage, 8)}
	app := &application{
		bot: newTestBot(t, telegramFake, "123:SECRET"), rtorrent: client,
		logger: log.New(io.Discard, "", 0), token: "123:SECRET",
		interval: 50 * time.Millisecond, duration: 2, sorts: make(map[int64]sortPreference),
	}
	app.speed(context.Background(), 111)
	expectSent(t, telegramFake.sent, 111, "↓ 2.0 KiB ↑ 1.0 KiB")
	rtorrentFake.set(func(f *fakeRtorrent) { f.downRate = 4096 })
	app.wg.Wait()
	edits := 0
	for _, method := range telegramFake.methods {
		if method == "editMessageText" {
			edits++
		}
	}
	// One edit for the new speed, and a final one that marks the reply stale.
	if edits != 2 {
		t.Fatalf("got %d edits, want 2: %v", edits, telegramFake.methods)
	}
}
