package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	// rateLimited answers that many sendMessage calls with 429 and retryAfter.
	rateLimited  int
	retryAfter   int
	sendAttempts int
	documents    []sentDocument
}

type sentMessage struct {
	chatID int64
	text   string
}

type sentDocument struct {
	chatID   int64
	filename string
	content  string
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
		f.sendAttempts++
		if f.rateLimited > 0 {
			f.rateLimited--
			return response(http.StatusTooManyRequests, fmt.Sprintf(
				`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":%d}}`, f.retryAfter)), nil
		}
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
	case "sendDocument":
		chatID, _ := strconv.ParseInt(request.FormValue("chat_id"), 10, 64)
		upload, header, err := request.FormFile("document")
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(upload)
		upload.Close()
		if err != nil {
			return nil, err
		}
		f.documents = append(f.documents, sentDocument{chatID: chatID, filename: header.Filename, content: string(content)})
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

func TestWhoamiReportsUserAndChatIDs(t *testing.T) {
	fake := &fakeTelegram{sent: make(chan sentMessage, 1)}
	app := &application{
		bot: newTestBot(t, fake, "123:SECRET"), logger: log.New(io.Discard, "", 0), token: "123:SECRET",
		masters: principals{usernames: map[string]struct{}{"alice": {}}}, botUsername: "ThisBot",
	}
	app.handle(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: 4242, Username: "Alice"}, Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup},
		Text: "/whoami@ThisBot",
	}})
	message := <-fake.sent
	if message.chatID != -100 || !strings.HasPrefix(message.text, "User ID: 4242 (@Alice)\nChat ID: -100\n") {
		t.Fatalf("whoami reply = %+v", message)
	}
}

func TestUnauthorizedPrivateUsersAreLoggedOnce(t *testing.T) {
	fake := &fakeTelegram{}
	var logs bytes.Buffer
	app := &application{
		bot: newTestBot(t, fake, "123:SECRET"), logger: log.New(&logs, "", 0), token: "123:SECRET",
		masters: principals{ids: map[int64]struct{}{7: {}}},
	}
	stranger := &models.User{ID: 555, Username: "mallory"}
	for _, chat := range []models.Chat{
		{ID: 555, Type: models.ChatTypePrivate},
		{ID: 555, Type: models.ChatTypePrivate},
		{ID: -100, Type: models.ChatTypeGroup},
	} {
		app.handle(context.Background(), &models.Update{Message: &models.Message{From: stranger, Chat: chat, Text: "/list"}})
	}
	app.handle(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: 666}, Chat: models.Chat{ID: -100, Type: models.ChatTypeGroup}, Text: "/list",
	}})
	if got := strings.Count(logs.String(), "unauthorized Telegram user ID 555 (@mallory)"); got != 1 {
		t.Fatalf("logged the stranger %d times: %q", got, logs.String())
	}
	if strings.Contains(logs.String(), "666") || len(fake.methods) != 0 {
		t.Fatalf("group member logged or replied to: logs=%q methods=%v", logs.String(), fake.methods)
	}
}

func TestSendRetriesRateLimitedMessages(t *testing.T) {
	fake := &fakeTelegram{rateLimited: 2}
	var logs bytes.Buffer
	app := &application{bot: newTestBot(t, fake, "123:SECRET"), logger: log.New(&logs, "", 0), token: "123:SECRET"}
	if _, err := app.send(context.Background(), 111, "Completed: debian.iso"); err != nil {
		t.Fatal(err)
	}
	if fake.sendAttempts != 3 || !reflect.DeepEqual(fake.chatIDs, []int64{111}) {
		t.Fatalf("attempts=%d delivered to %v", fake.sendAttempts, fake.chatIDs)
	}
	if !strings.Contains(logs.String(), "rate limit") {
		t.Fatalf("retries were not logged: %q", logs.String())
	}
}

func TestSendGivesUpOnLongRateLimits(t *testing.T) {
	fake := &fakeTelegram{rateLimited: 1, retryAfter: 3600}
	app := &application{bot: newTestBot(t, fake, "123:SECRET"), logger: log.New(io.Discard, "", 0), token: "123:SECRET"}
	started := time.Now()
	_, err := app.send(context.Background(), 111, "hello")
	if err == nil || !strings.Contains(err.Error(), "retry_after 3600") {
		t.Fatalf("expected the rate-limit error, got %v", err)
	}
	if fake.sendAttempts != 1 || time.Since(started) > time.Second {
		t.Fatalf("attempts=%d after %s", fake.sendAttempts, time.Since(started))
	}
}

func TestSendAttachesLongRepliesAsFile(t *testing.T) {
	fake := &fakeTelegram{}
	app := &application{bot: newTestBot(t, fake, "123:SECRET"), logger: log.New(io.Discard, "", 0), token: "123:SECRET"}
	fits := strings.Repeat("x", maxTelegramMessage*maxMessageChunks)
	if _, err := app.send(context.Background(), 111, fits); err != nil {
		t.Fatal(err)
	}
	if fake.sendAttempts != maxMessageChunks || len(fake.documents) != 0 {
		t.Fatalf("%d-chunk reply: %d messages, %d files", maxMessageChunks, fake.sendAttempts, len(fake.documents))
	}

	long := strings.Repeat("<abcdef0> some.torrent.name\n", 1000)
	messageID, err := app.send(context.Background(), 222, long)
	if err != nil || messageID != 0 {
		t.Fatalf("messageID=%d err=%v", messageID, err)
	}
	want := []sentDocument{{chatID: 222, filename: "rtelegram.txt", content: long}}
	if fake.sendAttempts != maxMessageChunks || !reflect.DeepEqual(fake.documents, want) {
		t.Fatalf("long reply: %d messages, files %+v", fake.sendAttempts, fake.documents)
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

func TestTelegramUploadReachesRtorrentAsRawBytesWithoutToken(t *testing.T) {
	const token = "123:SUPERSECRET"
	file := []byte("d4:infod4:name4:testee")
	telegramFake := &fakeTelegram{file: file, sent: make(chan sentMessage, 4)}
	rtorrentFake, client := newFakeRtorrent(t)
	info := sha1.Sum([]byte("d4:name4:teste"))
	hash := strings.ToUpper(hex.EncodeToString(info[:]))
	rtorrentFake.onLoad(func(string) *rtapi.Torrent { return &rtapi.Torrent{Name: "test", Hash: hash} })
	app := &application{
		bot:          newTestBot(t, telegramFake, token),
		httpClient:   telegramFake,
		rtorrent:     client,
		logger:       log.New(io.Discard, "", 0),
		token:        token,
		downloadRoot: "/remote",
	}
	message := &models.Message{
		Chat:     models.Chat{ID: 111, Type: models.ChatTypePrivate},
		Document: &models.Document{FileID: "file", FileName: "a.torrent", FileSize: int64(len(file))},
	}
	app.receiveTorrent(context.Background(), 111, message, "d=/remote/path linux")
	expectSent(t, telegramFake.sent, 111, "Added: <"+strings.ToLower(hash[:7])+"> test")
	app.wg.Wait()

	loads := rtorrentFake.requestsContaining("load.raw")
	if len(loads) != 1 {
		t.Fatalf("got %d load.raw_start requests, want 1", len(loads))
	}
	for _, want := range []string{base64.StdEncoding.EncodeToString(file), "/remote/path", "linux"} {
		if !strings.Contains(loads[0], want) {
			t.Fatalf("raw load request is missing %q: %s", want, loads[0])
		}
	}
	if leaked := rtorrentFake.requestsContaining(token); len(leaked) != 0 {
		t.Fatalf("token crossed the rTorrent boundary: %s", leaked[0])
	}
}

func TestConfineDirectory(t *testing.T) {
	tests := []struct{ root, requested, want string }{
		{"/data", "movies", "/data/movies"},
		{"/data", "/data/tv/", "/data/tv"},
		{"/data", "/data", "/data"},
		{"/data/", "a/../b", "/data/b"},
		{"/", "/anywhere", "/anywhere"},
		{"/data", "/data/../etc", ""},
		{"/data", "../etc", ""},
		{"/data", "/datax", ""},
		{"/data", "/home/user/.ssh", ""},
		{"relative", "movies", ""},
		{"", "movies", ""},
	}
	for _, test := range tests {
		got, err := confineDirectory(test.root, test.requested)
		if got != test.want || (err == nil) != (test.want != "") {
			t.Errorf("confineDirectory(%q, %q) = %q, %v; want %q", test.root, test.requested, got, err, test.want)
		}
	}
}

func TestUploadDirectoriesStayInsideTheDownloadRoot(t *testing.T) {
	file := []byte("d4:infod4:name4:testee")
	tests := []struct {
		caption, root string
		directory     string // the d.directory.set argument, if loaded
		reply         string
	}{
		{"d=movies", "", `d.directory.set="/downloads/movies"`, ""},
		{"d=/downloads/tv", "", `d.directory.set="/downloads/tv"`, ""},
		{"d=/home/user/.ssh", "", "", "receiver: /home/user/.ssh is outside the download root /downloads; set -download-root to allow it"},
		{"d=/mnt/disk2/movies", "/mnt", `d.directory.set="/mnt/disk2/movies"`, ""},
	}
	for _, test := range tests {
		t.Run(test.caption, func(t *testing.T) {
			telegramFake := &fakeTelegram{file: file, sent: make(chan sentMessage, 4)}
			rtorrentFake, client := newFakeRtorrent(t)
			app := &application{
				bot: newTestBot(t, telegramFake, "123:SECRET"), httpClient: telegramFake, rtorrent: client,
				logger: log.New(io.Discard, "", 0), token: "123:SECRET", downloadRoot: test.root,
			}
			message := &models.Message{
				Chat:     models.Chat{ID: 111, Type: models.ChatTypePrivate},
				Document: &models.Document{FileID: "file", FileName: "a.torrent", FileSize: int64(len(file))},
			}
			app.receiveTorrent(context.Background(), 111, message, test.caption)
			app.wg.Wait()
			loads := rtorrentFake.loadCalls()
			if test.reply != "" {
				expectSent(t, telegramFake.sent, 111, test.reply)
				if len(loads) != 0 {
					t.Fatalf("a refused directory reached rTorrent: %v", loads)
				}
				return
			}
			if len(loads) != 1 || !slices.Contains(loads[0], test.directory) {
				t.Fatalf("load calls = %q, want one with %s", loads, test.directory)
			}
		})
	}
}

func TestUploadRejectsInvalidTorrentFiles(t *testing.T) {
	telegramFake := &fakeTelegram{file: []byte("<html>not a torrent</html>"), sent: make(chan sentMessage, 4)}
	rtorrentFake, client := newFakeRtorrent(t)
	app := &application{
		bot: newTestBot(t, telegramFake, "123:SECRET"), httpClient: telegramFake, rtorrent: client,
		logger: log.New(io.Discard, "", 0), token: "123:SECRET",
	}
	message := &models.Message{
		Chat:     models.Chat{ID: 111, Type: models.ChatTypePrivate},
		Document: &models.Document{FileID: "file", FileName: "bad.torrent", FileSize: 26},
	}
	app.receiveTorrent(context.Background(), 111, message, "")
	expectSent(t, telegramFake.sent, 111, "receiver: bad.torrent is not a valid torrent file: not a bencoded dictionary")
	if len(rtorrentFake.loadCalls()) != 0 {
		t.Fatal("an invalid file was sent to rTorrent")
	}
}

func TestTorrentInfoHashHashesTheInfoDictionary(t *testing.T) {
	info := "d6:lengthi5e4:name5:a.txt12:piece lengthi16384e6:pieces20:" + strings.Repeat("x", 20) + "e"
	sum := sha1.Sum([]byte(info))
	want := strings.ToUpper(hex.EncodeToString(sum[:]))
	if got, err := torrentInfoHash([]byte("d8:announce20:udp://tracker:80/ann4:info" + info + "e")); err != nil || got != want {
		t.Fatalf("torrentInfoHash = %q, %v; want %q", got, err, want)
	}
	for _, bad := range []string{
		"", "not a torrent", "d8:announce", "d4:infoi1ee", "d4:infod4:name", "d99:info",
		"d4:info" + strings.Repeat("l", 100) + strings.Repeat("e", 100) + "e",
	} {
		if hash, err := torrentInfoHash([]byte(bad)); err == nil {
			t.Fatalf("torrentInfoHash(%q) = %q, want an error", bad, hash)
		}
	}
}

func TestMagnetInfo(t *testing.T) {
	const hash = "1C60CBECF4C632EDC7AB546623454B33A295CCEA"
	raw, _ := hex.DecodeString(hash)
	tests := []struct{ source, hash, name string }{
		{"magnet:?xt=urn:btih:" + strings.ToLower(hash) + "&dn=Debian+12&tr=udp%3A%2F%2Ftracker", hash, "Debian 12"},
		{"magnet:?xt=urn:btih:" + base32.StdEncoding.EncodeToString(raw), hash, "magnet 1c60cbe"},
		{"MAGNET:?dn=v2+only&xt=urn:btmh:1220" + strings.Repeat("ab", 32), "", "v2 only"},
		{"magnet:?xt=urn:btih:nothex", "", "magnet link"},
		{"https://tracker.example/a.torrent?passkey=secret", "", ""},
	}
	for _, test := range tests {
		if hash, name := magnetInfo(test.source); hash != test.hash || name != test.name {
			t.Errorf("magnetInfo(%q) = %q, %q; want %q, %q", test.source, hash, name, test.hash, test.name)
		}
	}
}

func TestSourceNameOmitsQueryStrings(t *testing.T) {
	for source, want := range map[string]string{
		"https://tracker.example/download/linux.torrent?passkey=secret": "linux.torrent",
		"https://tracker.example/?id=1&passkey=secret":                  "tracker.example",
		"/home/user/watch/local.torrent":                                "local.torrent",
	} {
		if got := sourceName(source); got != want {
			t.Errorf("sourceName(%q) = %q, want %q", source, got, want)
		}
	}
}

func TestAddConfirmsWhatRtorrentLoaded(t *testing.T) {
	const magnetHash = "1C60CBECF4C632EDC7AB546623454B33A295CCEA"
	magnet := "magnet:?xt=urn:btih:" + magnetHash + "&dn=Debian+12"
	link := "https://tracker.example/dl/linux.torrent?passkey=secret"
	tests := []struct {
		name    string
		source  string
		loaded  rtapi.Torrents
		loads   *rtapi.Torrent
		reply   string
		request bool
	}{
		{"magnet appears", magnet, nil, &rtapi.Torrent{Name: "Debian 12", Hash: magnetHash},
			"Added: <1c60cbe> Debian 12", true},
		{"magnet already loaded", magnet, rtapi.Torrents{{Name: "Debian 12", Hash: magnetHash}}, nil,
			"add: Debian 12 is already loaded", false},
		{"link appears", link, nil, &rtapi.Torrent{Name: "linux", Hash: strings.Repeat("B", 40)},
			"Added: <bbbbbbb> linux", true},
		{"nothing appears", link, nil, nil,
			"add: rTorrent did not load linux.torrent within 0s", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rtorrentFake, client := newFakeRtorrent(t, test.loaded...)
			rtorrentFake.onLoad(func(string) *rtapi.Torrent { return test.loads })
			telegramFake := &fakeTelegram{sent: make(chan sentMessage, 4)}
			app := &application{
				bot: newTestBot(t, telegramFake, "123:SECRET"), rtorrent: client,
				logger: log.New(io.Discard, "", 0), token: "123:SECRET",
			}
			app.add(context.Background(), 111, []string{test.source})
			select {
			case message := <-telegramFake.sent:
				if !strings.HasPrefix(message.text, test.reply) || strings.Contains(message.text, "secret") {
					t.Fatalf("reply = %q, want prefix %q without the passkey", message.text, test.reply)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("add sent no reply")
			}
			app.wg.Wait()
			if requested := len(rtorrentFake.loadCalls()) != 0; requested != test.request {
				t.Fatalf("load requested = %v, want %v", requested, test.request)
			}
		})
	}
}

func TestDeldataRefusesUnknownOrSharedData(t *testing.T) {
	targetHash := strings.Repeat("A", 40)
	otherHash := strings.Repeat("B", 40)
	// Each case returns the loaded torrents, target first. Every path a case
	// names exists on disk, as real torrent data would.
	tests := []struct {
		name     string
		torrents func(root, show string) rtapi.Torrents
		reply    string
		deleted  bool
	}{
		{"opened torrent nested in the data", func(root, show string) rtapi.Torrents {
			return rtapi.Torrents{
				{Name: "show", Hash: targetHash, Path: show},
				{Name: "extra", Hash: otherHash, Path: filepath.Join(show, "extra")},
			}
		}, "deldata: torrent data overlaps extra", false},
		{"unopened multi-file cross-seed", func(root, show string) rtapi.Torrents {
			return rtapi.Torrents{
				{Name: "show", Hash: targetHash, Path: show},
				{Name: "show", Hash: otherHash, Directory: show, MultiFile: true},
			}
		}, "deldata: torrent data overlaps show", false},
		{"unopened single-file inside the data", func(root, show string) rtapi.Torrents {
			return rtapi.Torrents{
				{Name: "show", Hash: targetHash, Path: show},
				{Name: "episode.mkv", Hash: otherHash, Directory: show},
			}
		}, "deldata: torrent data overlaps episode.mkv", false},
		{"unopened single-file beside the data", func(root, show string) rtapi.Torrents {
			return rtapi.Torrents{
				{Name: "show", Hash: targetHash, Path: show},
				{Name: "movie.mkv", Hash: otherHash, Directory: root},
			}
		}, "Deleted with data: show", true},
		{"other location unknown", func(root, show string) rtapi.Torrents {
			return rtapi.Torrents{
				{Name: "show", Hash: targetHash, Path: show},
				{Name: "lost", Hash: otherHash},
			}
		}, "deldata: rTorrent did not report where lost", false},
		{"single-file name rewritten by libtorrent", func(root, show string) rtapi.Torrents {
			return rtapi.Torrents{
				{Name: "show", Hash: targetHash, Path: show},
				{Name: "a/b", Hash: otherHash, Directory: root},
			}
		}, "deldata: rTorrent did not report where a/b", false},
		{"unopened target", func(root, show string) rtapi.Torrents {
			return rtapi.Torrents{{Name: "show", Hash: targetHash, Directory: show, MultiFile: true}}
		}, "Deleted with data: show", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			show := filepath.Join(root, "show")
			if err := os.MkdirAll(filepath.Join(show, "extra"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{filepath.Join(show, "episode.mkv"), filepath.Join(root, "movie.mkv")} {
				if err := os.WriteFile(file, []byte(file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rtorrentFake, client := newFakeRtorrent(t, test.torrents(root, show)...)
			telegramFake := &fakeTelegram{sent: make(chan sentMessage, 4)}
			app := &application{
				bot:      newTestBot(t, telegramFake, "123:SECRET"),
				rtorrent: client,
				logger:   log.New(io.Discard, "", 0),
				token:    "123:SECRET",
				dataRoot: root,
			}
			app.deldata(context.Background(), 111, []string{targetHash, "confirm"})

			select {
			case message := <-telegramFake.sent:
				if !strings.HasPrefix(message.text, test.reply) {
					t.Fatalf("reply = %q, want prefix %q", message.text, test.reply)
				}
			default:
				t.Fatal("deldata sent no reply")
			}
			erased := len(rtorrentFake.called("d.erase")) != 0
			_, err := os.Stat(show)
			if erased != test.deleted || os.IsNotExist(err) != test.deleted {
				t.Fatalf("erased=%v data removed=%v, want both %v", erased, os.IsNotExist(err), test.deleted)
			}
			if _, err := os.Stat(filepath.Join(root, "movie.mkv")); err != nil {
				t.Fatalf("sibling data was damaged: %v", err)
			}
		})
	}
}

// referenceHashPrefixes is the original quadratic definition, kept as an
// oracle for the sorted implementation.
func referenceHashPrefixes(torrents rtapi.Torrents) map[string]string {
	result := make(map[string]string, len(torrents))
	for _, torrent := range torrents {
		hash := strings.ToLower(strings.TrimSpace(torrent.Hash))
		if hash == "" {
			continue
		}
		length := min(7, len(hash))
		for length < len(hash) {
			unique := true
			for _, other := range torrents {
				if other != torrent && strings.HasPrefix(strings.ToLower(strings.TrimSpace(other.Hash)), hash[:length]) {
					unique = false
					break
				}
			}
			if unique {
				break
			}
			length++
		}
		result[hash] = hash[:length]
	}
	return result
}

func randomTorrents(count int) rtapi.Torrents {
	random := rand.New(rand.NewPCG(1, 2))
	torrents := make(rtapi.Torrents, count)
	for i := range torrents {
		hash := fmt.Sprintf("%016X%016X%08X", random.Uint64(), random.Uint64(), random.Uint32())
		torrents[i] = &rtapi.Torrent{Name: fmt.Sprint("torrent ", i), Hash: hash}
	}
	return torrents
}

func TestHashPrefixesMatchQuadraticReference(t *testing.T) {
	torrents := randomTorrents(500)
	random := rand.New(rand.NewPCG(3, 4))
	for i := 1; i < len(torrents); i += 3 {
		// Share a 7-12 character prefix with an earlier hash.
		prefix := torrents[random.IntN(i)].Hash[:7+random.IntN(6)]
		torrents[i].Hash = prefix + torrents[i].Hash[len(prefix):]
	}
	torrents[2].Hash = strings.ToLower(torrents[2].Hash)
	torrents = append(torrents,
		&rtapi.Torrent{Hash: torrents[10].Hash},
		&rtapi.Torrent{Hash: " " + torrents[20].Hash[:12] + " "},
		&rtapi.Torrent{},
	)
	got, want := hashPrefixes(torrents), referenceHashPrefixes(torrents)
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes, want %d", len(got), len(want))
	}
	for hash, prefix := range want {
		if got[hash] != prefix {
			t.Fatalf("prefix for %s = %q, want %q", hash, got[hash], prefix)
		}
	}
}

func BenchmarkHashPrefixes(b *testing.B) {
	torrents := randomTorrents(5000)
	for b.Loop() {
		hashPrefixes(torrents)
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
