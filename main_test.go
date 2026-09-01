package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

func TestParsePrincipalsAndAuthorization(t *testing.T) {
	masters, legacy, err := parsePrincipals("123, @Alice,456")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy, []string{"alice"}) {
		t.Fatalf("legacy usernames = %v", legacy)
	}
	for _, user := range []*models.User{{ID: 123}, {Username: "ALICE"}} {
		if !masters.authorized(user) {
			t.Fatalf("expected %+v to be authorized", user)
		}
	}
	for _, user := range []*models.User{nil, {}, {ID: 999}, {Username: "mallory"}} {
		if masters.authorized(user) {
			t.Fatalf("expected %+v to be rejected", user)
		}
	}
	for _, input := range []string{"", "alice,", "@", ",,", "alice,  ,bob", "0"} {
		if _, _, err := parsePrincipals(input); err == nil {
			t.Fatalf("parsePrincipals(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseCommandRequiresSlashInGroupsAndSupportsSuffixes(t *testing.T) {
	tests := []struct {
		name    string
		kind    models.ChatType
		text    string
		command string
		args    []string
		ok      bool
	}{
		{"private bare", models.ChatTypePrivate, "  stop   all ", "stop", []string{"all"}, true},
		{"group bare rejected", models.ChatTypeGroup, "stop all", "", nil, false},
		{"group slash", models.ChatTypeGroup, "/stop all", "stop", []string{"all"}, true},
		{"bot suffix", models.ChatTypeSupergroup, "/list@ThisBot tracker", "list", []string{"tracker"}, true},
		{"other bot", models.ChatTypeGroup, "/list@OtherBot", "", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := &models.Message{Chat: models.Chat{Type: test.kind}, Text: test.text}
			command, args, ok := parseCommand(message, "ThisBot")
			if command != test.command || ok != test.ok || !reflect.DeepEqual(args, test.args) {
				t.Fatalf("got command=%q args=%v ok=%v", command, args, ok)
			}
		})
	}
}

func TestGroupDocumentsRequireAnAddCaption(t *testing.T) {
	document := &models.Document{FileID: "file", FileName: "a.torrent", FileSize: 4}
	tests := []struct {
		name    string
		message *models.Message
		options string
		ok      bool
	}{
		{"private options", &models.Message{Chat: models.Chat{Type: models.ChatTypePrivate}, Document: document, Caption: "d=/remote linux"}, "d=/remote linux", true},
		{"group bare", &models.Message{Chat: models.Chat{Type: models.ChatTypeGroup}, Document: document, Caption: "d=/remote linux"}, "", false},
		{"group command", &models.Message{Chat: models.Chat{Type: models.ChatTypeGroup}, Document: document, Caption: "/add@ThisBot d=/remote linux"}, "d=/remote linux", true},
		{"other bot", &models.Message{Chat: models.Chat{Type: models.ChatTypeGroup}, Document: document, Caption: "/add@OtherBot"}, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, ok := documentOptions(test.message, "ThisBot")
			if options != test.options || ok != test.ok {
				t.Fatalf("options=%q ok=%v", options, ok)
			}
		})
	}

	fake := &fakeTelegram{}
	app := &application{
		bot:         newTestBot(t, fake, "123:SECRET"),
		httpClient:  fake,
		logger:      log.New(io.Discard, "", 0),
		token:       "123:SECRET",
		botUsername: "ThisBot",
		masters:     principals{ids: map[int64]struct{}{123: {}}},
	}
	message := tests[1].message
	message.From = &models.User{ID: 123}
	app.handle(context.Background(), &models.Update{Message: message})
	if fake.getFileCalls != 0 {
		t.Fatalf("bare group document made %d file requests", fake.getFileCalls)
	}
}

func TestChunkMessagePreservesUTF8AndBounds(t *testing.T) {
	text := strings.Repeat("界", 5000) + "\n" + strings.Repeat("x", 5000)
	chunks := chunkMessage(text)
	if strings.Join(chunks, "") != text {
		t.Fatal("chunks do not reconstruct the original text")
	}
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) || utf8.RuneCountInString(chunk) > maxTelegramMessage {
			t.Fatalf("chunk %d is invalid or too large: %d runes", i, utf8.RuneCountInString(chunk))
		}
	}
	if got := chunkMessage(strings.Repeat("a", maxTelegramMessage+1)); len(got) != 2 {
		t.Fatalf("long line produced %d chunks", len(got))
	}
}

func TestStableHashPrefixesResolveAcrossReorder(t *testing.T) {
	first := &rtapi.Torrent{Name: "first", Hash: "abcdef0123456789"}
	second := &rtapi.Torrent{Name: "second", Hash: "abcdef0999999999"}
	torrents := rtapi.Torrents{first, second}
	prefixes := hashPrefixes(torrents)
	ref := torrentRef(first, prefixes)
	if len(ref) <= 7 {
		t.Fatalf("colliding prefix was not extended: %q", ref)
	}
	resolved, err := resolveTorrent(rtapi.Torrents{second, first}, ref)
	if err != nil || resolved != first {
		t.Fatalf("resolved %v, %v", resolved, err)
	}
	selected, err := selectTorrents(torrents, []string{ref, torrentRef(second, prefixes)}, false)
	if err != nil || len(selected) != 2 {
		t.Fatalf("selected %d torrents: %v", len(selected), err)
	}
	if _, err := resolveTorrent(torrents, "abcdef0"); err == nil {
		t.Fatal("ambiguous prefix was accepted")
	}
	if _, err := selectTorrents(nil, []string{"all"}, true); err == nil {
		t.Fatal("empty all-selection reported a successful mutation")
	}
}

func TestDataRootContainmentAndRemoval(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "downloads")
	child := filepath.Join(root, "torrent")
	sibling := filepath.Join(base, "keep")
	for _, path := range []string{child, sibling} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := deletionRelative(root, child); err != nil {
		t.Fatalf("safe child rejected: %v", err)
	}
	for _, unsafe := range []string{"relative", root, base, sibling} {
		if _, err := deletionRelative(root, unsafe); err == nil {
			t.Fatalf("unsafe target %q accepted", unsafe)
		}
	}
	if !pathsOverlap(child, filepath.Join(child, "nested")) || pathsOverlap(child, sibling) {
		t.Fatal("path overlap detection is incorrect")
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(".", alias); err == nil {
		if !pathsOverlap(filepath.Join(alias, "torrent"), child) {
			t.Fatal("in-root symlink alias bypassed overlap detection")
		}
		terminal := filepath.Join(root, "terminal-link")
		if err := os.Symlink("torrent", terminal); err != nil {
			t.Fatal(err)
		}
		opened, _, err := validateTorrentData(root, terminal)
		if opened != nil {
			opened.Close()
		}
		if err == nil {
			t.Fatal("terminal symlink was accepted for recursive removal")
		}
	} else {
		t.Logf("symlink checks unavailable: %v", err)
	}
	opened, relative, err := validateTorrentData(root, child)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.RemoveAll(relative); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	opened.Close()
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Fatalf("child still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sibling, "sentinel")); err != nil {
		t.Fatalf("sibling was damaged: %v", err)
	}

	link := filepath.Join(root, "escape")
	if err := os.Symlink(sibling, link); err == nil {
		opened, _, err := validateTorrentData(root, link)
		if opened != nil {
			opened.Close()
		}
		if err == nil {
			t.Fatal("symlink escape was accepted")
		}
		if _, err := os.Stat(filepath.Join(sibling, "sentinel")); err != nil {
			t.Fatalf("symlink escape damaged sibling: %v", err)
		}
	}
}

type fakeTelegram struct {
	chatIDs      []int64
	threadIDs    []int
	methods      []string
	file         []byte
	getFileCalls int
	sent         chan sentMessage
}

type sentMessage struct {
	chatID int64
	text   string
}

type errorHTTPClient struct{ err error }

func (c errorHTTPClient) Do(*http.Request) (*http.Response, error) { return nil, c.err }

func (f *fakeTelegram) Do(request *http.Request) (*http.Response, error) {
	method := filepath.Base(request.URL.Path)
	if request.Method == http.MethodGet {
		return response(http.StatusOK, string(f.file)), nil
	}
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		return nil, err
	}
	var result string
	switch method {
	case "sendChatAction":
		result = "true"
	case "sendMessage":
		chatID, _ := strconv.ParseInt(request.FormValue("chat_id"), 10, 64)
		threadID, _ := strconv.Atoi(request.FormValue("message_thread_id"))
		f.chatIDs = append(f.chatIDs, chatID)
		f.threadIDs = append(f.threadIDs, threadID)
		f.methods = append(f.methods, method)
		if f.sent != nil {
			f.sent <- sentMessage{chatID: chatID, text: request.FormValue("text")}
		}
		result = fmt.Sprintf(`{"message_id":%d,"date":0,"chat":{"id":%d,"type":"private"}}`, len(f.chatIDs), chatID)
	case "editMessageText":
		chatID, _ := strconv.ParseInt(request.FormValue("chat_id"), 10, 64)
		f.chatIDs = append(f.chatIDs, chatID)
		f.methods = append(f.methods, method)
		result = fmt.Sprintf(`{"message_id":1,"date":0,"chat":{"id":%d,"type":"private"}}`, chatID)
	case "getFile":
		f.getFileCalls++
		result = `{"file_id":"file","file_unique_id":"unique","file_size":4,"file_path":"files/a.torrent"}`
	default:
		return nil, fmt.Errorf("unexpected Telegram method %q", method)
	}
	return response(http.StatusOK, `{"ok":true,"result":`+result+`}`), nil
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func newTestBot(t *testing.T, client telegram.HttpClient, token string) *telegram.Bot {
	t.Helper()
	bot, err := telegram.New(token, telegram.WithSkipGetMe(), telegram.WithHTTPClient(time.Second, client))
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func TestSendRoutesToExplicitChat(t *testing.T) {
	fake := &fakeTelegram{}
	app := &application{
		bot: newTestBot(t, fake, "123:SECRET"), logger: log.New(io.Discard, "", 0), token: "123:SECRET",
		masters: principals{ids: map[int64]struct{}{7: {}}}, botUsername: "rtelegram_bot",
	}
	for _, chatID := range []int64{111, 222} {
		if _, err := app.send(context.Background(), chatID, "hello"); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.edit(context.Background(), 333, 1, "updated"); err != nil {
		t.Fatal(err)
	}
	app.handle(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: 7}, Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup},
		MessageThreadID: 42, Text: "/help",
	}})
	if !reflect.DeepEqual(fake.chatIDs, []int64{111, 222, 333, -100}) {
		t.Fatalf("messages routed to %v", fake.chatIDs)
	}
	if !reflect.DeepEqual(fake.threadIDs, []int{0, 0, 42}) {
		t.Fatalf("message threads = %v", fake.threadIDs)
	}
	if !reflect.DeepEqual(fake.methods, []string{"sendMessage", "sendMessage", "editMessageText", "sendMessage"}) {
		t.Fatalf("Telegram methods = %v", fake.methods)
	}
}

func TestBatchMutationErrorsWarnAboutPartialApplication(t *testing.T) {
	err := errors.New("second item faulted")
	if got := mutationError("del", 1, err); strings.Contains(got, "some torrents") {
		t.Fatalf("single mutation reported a partial batch: %q", got)
	}
	if got := mutationError("del", 2, err); !strings.Contains(got, "some torrents") || !strings.Contains(got, "refresh") {
		t.Fatalf("batch mutation hid partial application: %q", got)
	}
}

func TestEditChangedSkipsUnchangedTelegramRequests(t *testing.T) {
	fake := &fakeTelegram{}
	app := &application{bot: newTestBot(t, fake, "123:SECRET"), logger: log.New(io.Discard, "", 0), token: "123:SECRET"}
	current := "same"
	if err := app.editChanged(context.Background(), 111, 1, &current, "same"); err != nil {
		t.Fatal(err)
	}
	if len(fake.methods) != 0 {
		t.Fatalf("unchanged text made Telegram calls: %v", fake.methods)
	}
	if err := app.editChanged(context.Background(), 111, 1, &current, "changed"); err != nil {
		t.Fatal(err)
	}
	if current != "changed" || !reflect.DeepEqual(fake.methods, []string{"editMessageText"}) {
		t.Fatalf("current=%q methods=%v", current, fake.methods)
	}
}

func TestSendRedactsTokenFromErrorsAndLogs(t *testing.T) {
	const token = "123:SUPERSECRET"
	var logs bytes.Buffer
	client := errorHTTPClient{err: fmt.Errorf("request containing %s failed", token)}
	app := &application{bot: newTestBot(t, client, token), logger: log.New(&logs, "", 0), token: token}
	_, err := app.send(context.Background(), 111, "hello")
	if err == nil {
		t.Fatal("send unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(logs.String(), token) {
		t.Fatalf("token leaked: error=%q logs=%q", err, logs.String())
	}
}

type fakeRawDownloader struct {
	data    []byte
	options *rtapi.DotTorrentWithOptions
}

func (f *fakeRawDownloader) DownloadRaw(data []byte, options *rtapi.DotTorrentWithOptions) error {
	f.data = bytes.Clone(data)
	copy := *options
	f.options = &copy
	return nil
}

type fakeMetadataDeleter struct {
	torrents rtapi.Torrents
}

func (f *fakeMetadataDeleter) DeleteMetadata(torrents ...*rtapi.Torrent) error {
	f.torrents = append(f.torrents, torrents...)
	return nil
}

func TestDataDeletionRequiresAcknowledgedMetadataCapability(t *testing.T) {
	torrent := &rtapi.Torrent{Hash: strings.Repeat("a", 40)}
	deleter := &fakeMetadataDeleter{}
	if err := deleteMetadata(deleter, torrent); err != nil {
		t.Fatal(err)
	}
	if len(deleter.torrents) != 1 || deleter.torrents[0] != torrent {
		t.Fatalf("metadata deletion received %#v", deleter.torrents)
	}
	if err := deleteMetadata(struct{}{}, torrent); err == nil {
		t.Fatal("old rtapi capability was allowed to precede local data removal")
	}
}

func TestTelegramUploadDownloadsLocallyWithoutPassingTokenToRtorrent(t *testing.T) {
	const token = "123:SUPERSECRET"
	fakeHTTP := &fakeTelegram{file: []byte("data")}
	app := &application{
		bot:        newTestBot(t, fakeHTTP, token),
		httpClient: fakeHTTP,
		logger:     log.New(io.Discard, "", 0),
		token:      token,
	}
	data, err := app.downloadTelegramFile(context.Background(), "file")
	if err != nil {
		t.Fatal(err)
	}
	raw := &fakeRawDownloader{}
	options := &rtapi.DotTorrentWithOptions{Name: "a.torrent", Dir: "/remote/path", Label: "linux"}
	if err := downloadRaw(raw, data, options); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw.data), token) || strings.Contains(raw.options.Link, token) || raw.options.Link != "" {
		t.Fatalf("token crossed the rtorrent boundary: data=%q link=%q", raw.data, raw.options.Link)
	}
	if err := downloadRaw(struct{}{}, data, options); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("unsafe old-rtapi fallback error: %v", err)
	}
}

func TestVersionExitsBeforeConfigurationOrNetwork(t *testing.T) {
	oldVersion := version
	version = "v9.9.9"
	defer func() { version = oldVersion }()
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, func(string) string { return "" }, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "v9.9.9\n" {
		t.Fatalf("version output = %q", stdout.String())
	}
}

func TestHelpExitsBeforeConfigurationOrNetwork(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, func(string) string { return "" }, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "-token") {
		t.Fatalf("help output = %q", stderr.String())
	}
}

func TestPlainRendererAndIECFormatter(t *testing.T) {
	torrent := &rtapi.Torrent{
		Name: "literal_*_[name]`", Hash: "abcdef012345", State: rtapi.Stopped,
		Completed: 1536, Percent: "50%", Ratio: 1.5,
	}
	text := formatTorrent(torrent, "abcdef0")
	for _, want := range []string{"literal_*_[name]`", "R: 1.50", "1.5 KiB"} {
		if !strings.Contains(text, want) {
			t.Fatalf("renderer %q does not contain %q", text, want)
		}
	}
	if strings.Contains(text, "%!") {
		t.Fatalf("formatter diagnostic leaked: %q", text)
	}
}

type firstWrite struct {
	once  sync.Once
	ready chan struct{}
}

func (w *firstWrite) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(data), nil
}

func expectSent(t *testing.T, messages <-chan sentMessage, chatID int64, text string) {
	t.Helper()
	select {
	case message := <-messages:
		if message.chatID != chatID || message.text != text {
			t.Fatalf("message = %+v, want chat=%d text=%q", message, chatID, text)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %q", text)
	}
}

func TestCompletedLogHandlesLateCreationFragmentsAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completed.log")
	messages := make(chan sentMessage, 8)
	fake := &fakeTelegram{sent: messages}
	started := &firstWrite{ready: make(chan struct{})}
	app := &application{
		bot:          newTestBot(t, fake, "123:SECRET"),
		logger:       log.New(started, "", 0),
		token:        "123:SECRET",
		notifyChatID: 987,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.watchCompletedLogEvery(ctx, path, 5*time.Millisecond, 5*time.Millisecond)
	}()
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-started.ready:
	case <-time.After(time.Second):
		t.Fatal("watcher did not attempt to open the missing file")
	}
	if err := os.WriteFile(path, []byte("created\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectSent(t, messages, 987, "Completed: created")

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("part"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := file.WriteString("ial\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	expectSent(t, messages, 987, "Completed: partial")

	if err := os.Rename(path, path+".1"); err == nil {
		if err := os.WriteFile(path, []byte("rotated\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		expectSent(t, messages, 987, "Completed: rotated")
	} else {
		t.Logf("open-file replacement unavailable; testing truncation instead: %v", err)
		if err := os.WriteFile(path, []byte("truncated\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		expectSent(t, messages, 987, "Completed: truncated")
	}
}
