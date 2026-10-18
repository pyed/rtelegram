package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

func TestDataRootDefaultsToALocalRTorrentsDirectory(t *testing.T) {
	downloads := t.TempDir()
	rtorrentFake, client := newFakeRtorrent(t)
	ctx := context.Background()
	local := config{rtorrentAddress: "localhost:5000"}

	rtorrentFake.set(func(f *fakeRtorrent) { f.directory = downloads + string(filepath.Separator) })
	if root, note, pending := chooseDataRoot(ctx, local, client); root != downloads || !strings.Contains(note, "rTorrent's download directory") || pending {
		t.Fatalf("local rTorrent: %q, %q", root, note)
	}
	file := filepath.Join(downloads, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// rTorrent's own default is "./", relative to where rTorrent runs; "."
	// exists here, but means rtelegram's directory.
	for _, directory := range []string{"./", ".", "~/downloads", filepath.Join(downloads, "missing"), file, ""} {
		rtorrentFake.set(func(f *fakeRtorrent) { f.directory = directory })
		if root, note, pending := chooseDataRoot(ctx, local, client); root != "" || !strings.Contains(note, "set -data-root") || pending {
			t.Errorf("rTorrent directory %q: %q, %q", directory, root, note)
		}
	}

	// Paths from another machine mean nothing here, so rTorrent is not asked.
	before := len(rtorrentFake.called("directory.default"))
	remote := config{rtorrentAddress: "https://user:pass@seedbox.example/RPC2"}
	if root, note, _ := chooseDataRoot(ctx, remote, client); root != "" || !strings.Contains(note, "another machine") {
		t.Fatalf("remote rTorrent: %q, %q", root, note)
	}
	if len(rtorrentFake.called("directory.default")) != before {
		t.Fatal("asked a remote rTorrent for its directory")
	}

	explicit := config{rtorrentAddress: "seedbox:5000", dataRoot: downloads}
	if root, _, _ := chooseDataRoot(ctx, explicit, client); root != downloads {
		t.Fatalf("explicit -data-root = %q", root)
	}
	off := config{rtorrentAddress: "localhost:5000", dataRoot: dataRootOff}
	if root, note, _ := chooseDataRoot(ctx, off, client); root != "" || !strings.Contains(note, "off") {
		t.Fatalf("-data-root off = %q, %q", root, note)
	}
}

// rTorrent may start after rtelegram. Until it answers, /get and deldata
// are off; once it says where it keeps data, they work there.
func TestDataRootWaitsForRTorrent(t *testing.T) {
	downloads := t.TempDir()
	rtorrentFake, client := newFakeRtorrent(t)
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.directory = downloads
		f.faults = map[string]bool{"directory.default": true}
	})
	cfg := config{rtorrentAddress: "localhost:5000"}
	if root, note, pending := chooseDataRoot(context.Background(), cfg, client); root != "" || !pending || !strings.Contains(note, "until rTorrent answers") {
		t.Fatalf("rTorrent not answering: %q, %q, %v", root, note, pending)
	}

	var logs bytes.Buffer
	app := &application{rtorrent: client, logger: log.New(&logs, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		app.resolveDataRoot(ctx, cfg, 10*time.Millisecond)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if root := app.dataRootPath(); root != "" {
		t.Fatalf("data root while rTorrent does not answer = %q", root)
	}
	rtorrentFake.set(func(f *fakeRtorrent) { f.faults = nil })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the data root was not resolved once rTorrent answered")
	}
	if root := app.dataRootPath(); root != downloads || !strings.Contains(logs.String(), "rTorrent answered: /get and deldata work beneath") {
		t.Fatalf("data root = %q, log %q", root, logs.String())
	}
}

// The log warns of an SCGI port, which anyone who can connect to it can
// control rTorrent through, and of secrets sent readably over the network.
func TestSecurityWarnings(t *testing.T) {
	for _, test := range []struct {
		cfg  config
		want []string
	}{
		{config{rtorrentAddress: "/run/rtorrent/rpc.socket"}, nil},
		{config{rtorrentAddress: `C:\rtorrent\rpc.socket`}, nil},
		{config{rtorrentAddress: "localhost:5000"}, []string{"SCGI port localhost:5000 takes commands"}},
		{config{rtorrentAddress: "seedbox:5000"}, []string{"SCGI port seedbox:5000"}},
		{config{rtorrentAddress: "https://user:pass@seedbox.example/RPC2"}, nil},
		{config{rtorrentAddress: "http://seedbox.example/RPC2"}, nil},
		{config{rtorrentAddress: "http://user:pass@localhost/RPC2"}, nil},
		{config{rtorrentAddress: "http://user:pass@seedbox.example/RPC2"}, []string{"rTorrent's password crosses the network"}},
		{config{rtorrentAddress: "/rpc.socket", indexerURL: "http://localhost:9696/1/api"}, nil},
		{config{rtorrentAddress: "/rpc.socket", indexerURL: "https://indexer.example/api"}, nil},
		{config{rtorrentAddress: "/rpc.socket", indexerURL: "http://indexer.example/api"}, []string{"indexer's API key crosses the network"}},
	} {
		got := securityWarnings(test.cfg)
		if len(got) != len(test.want) {
			t.Errorf("%+v: warnings %q, want %d", test.cfg, got, len(test.want))
			continue
		}
		for i, want := range test.want {
			if !strings.Contains(got[i], want) {
				t.Errorf("%+v: warning %q lacks %q", test.cfg, got[i], want)
			}
		}
	}
}

func TestLocalAddress(t *testing.T) {
	for address, want := range map[string]bool{
		"/run/rtorrent/rpc.socket":               true,
		"localhost:5000":                         true,
		"LOCALHOST:5000":                         true,
		"127.0.0.1:5000":                         true,
		"[::1]:5000":                             true,
		":5000":                                  true,
		"http://localhost/RPC2":                  true,
		"http://127.0.0.1:8080/RPC2":             true,
		"seedbox:5000":                           false,
		"10.0.0.2:5000":                          false,
		"https://user:pass@seedbox.example/RPC2": false,
		"http://192.168.1.5/RPC2":                false,
	} {
		if got := localAddress(address); got != want {
			t.Errorf("localAddress(%q) = %v, want %v", address, got, want)
		}
	}
}

func TestSetupMasterRepliesWithTheUsersID(t *testing.T) {
	stranger := &models.Message{From: &models.User{ID: 555}, Chat: models.Chat{ID: 555, Type: models.ChatTypePrivate}, Text: "hi"}
	for _, test := range []struct {
		masters map[int64]struct{}
		reply   bool
	}{
		{map[int64]struct{}{setupMaster: {}}, true},
		{map[int64]struct{}{setupMaster: {}, 7: {}}, false},
		{map[int64]struct{}{7: {}}, false},
	} {
		telegramFake := &fakeTelegram{sent: make(chan sentMessage, 4)}
		var logs bytes.Buffer
		app := &application{bot: newTestBot(t, telegramFake, "123:SECRET"), logger: log.New(&logs, "", 0), token: "123:SECRET",
			masters: principals{ids: test.masters}}
		app.handle(context.Background(), &models.Update{Message: stranger})
		app.handle(context.Background(), &models.Update{Message: stranger}) // answered once
		sent := drain(telegramFake)
		if test.reply != (len(sent) == 1 && strings.HasPrefix(sent[0], "Your Telegram user ID is 555. Restart rtelegram with RT_MASTERS=555")) {
			t.Errorf("masters %v: sent %q", test.masters, sent)
		}
		if !strings.Contains(logs.String(), "Telegram user ID 555 is not a master") {
			t.Errorf("masters %v: log %q", test.masters, logs.String())
		}
	}
}

func TestDataRootOffAndArgumentHints(t *testing.T) {
	getenv := testEnv(map[string]string{"RT_TOKEN": "123:SECRET", "RT_MASTERS": "7"})
	if cfg, err := parseConfig([]string{"-data-root", "off"}, getenv, io.Discard); err != nil || cfg.dataRoot != dataRootOff {
		t.Fatalf("-data-root off = %q, %v", cfg.dataRoot, err)
	}
	if _, err := parseConfig([]string{"-data-root", "downloads"}, getenv, io.Discard); err == nil {
		t.Fatal("a relative -data-root was accepted")
	}
	for args, want := range map[string]string{
		"-masters=7 - -data-root=/home/me/downloads": `a lone "-" ends the flags, so -data-root=/home/me/downloads was not read`,
		"—data-root=/home/me/downloads":              "starts with a long dash",
		"downloads":                                  "rtelegram takes only flags",
	} {
		if _, err := parseConfig(strings.Fields(args), getenv, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseConfig(%s) = %v, want %q", args, err, want)
		}
	}
}
