package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
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

// fakeRtorrent answers rtapi's SCGI requests from a fixed torrent list, so
// handlers run against a real *rtapi.Rtorrent. It records each request body.
type fakeRtorrent struct {
	t        *testing.T
	torrents rtapi.Torrents
	mu       sync.Mutex
	requests []string
}

func newFakeRtorrent(t *testing.T, torrents ...*rtapi.Torrent) (*fakeRtorrent, *rtapi.Rtorrent) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeRtorrent{t: t, torrents: torrents}
	var connections sync.WaitGroup
	connections.Add(1)
	go func() {
		defer connections.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer conn.Close()
				fake.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connections.Wait()
	})
	client, err := rtapi.NewRtorrent(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return fake, client
}

func (f *fakeRtorrent) serve(conn net.Conn) {
	body, err := readSCGIBody(conn)
	if err != nil {
		f.t.Errorf("read SCGI request: %v", err)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.mu.Unlock()
	if _, err := io.WriteString(conn, f.respond(body)); err != nil {
		f.t.Errorf("write SCGI response: %v", err)
	}
}

func (f *fakeRtorrent) requestsContaining(text string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matches []string
	for _, request := range f.requests {
		if strings.Contains(request, text) {
			matches = append(matches, request)
		}
	}
	return matches
}

func (f *fakeRtorrent) respond(body string) string {
	switch {
	case strings.Contains(body, "load.raw_start"):
		return xmlrpcResponse(xmlrpcInt(0))
	case strings.Contains(body, "system.client_version"):
		return xmlrpcResponse(xmlrpcArray(xmlrpcArray(xmlrpcString("0.9.8")), xmlrpcArray(xmlrpcString("0.13.8"))))
	case strings.Contains(body, "d.multicall2"):
		rows := make([]string, len(f.torrents))
		for i, torrent := range f.torrents {
			multiFile := 0
			if torrent.MultiFile {
				multiFile = 1
			}
			rows[i] = xmlrpcArray(
				xmlrpcString(torrent.Name), xmlrpcString(torrent.Hash),
				xmlrpcInt(0), xmlrpcInt(0), xmlrpcInt(1), xmlrpcInt(1), xmlrpcInt(0), xmlrpcInt(0), xmlrpcInt(0),
				xmlrpcString(""), xmlrpcString(torrent.Path), xmlrpcInt(0), xmlrpcString(""), xmlrpcInt(1),
				xmlrpcInt(0), xmlrpcString(""), xmlrpcString(torrent.Directory), xmlrpcInt(multiFile),
			)
		}
		return xmlrpcResponse(xmlrpcArray(rows...))
	case strings.Contains(body, ">t.url<"):
		return multicallResults(len(f.torrents), xmlrpcString(""))
	case strings.Contains(body, ">d.erase<"):
		return multicallResults(strings.Count(body, ">d.erase<"), xmlrpcInt(0))
	}
	f.t.Errorf("unexpected rTorrent request: %s", body)
	return ""
}

func readSCGIBody(r io.Reader) (string, error) {
	reader := bufio.NewReader(r)
	lengthText, err := reader.ReadString(':')
	if err != nil {
		return "", err
	}
	length, err := strconv.Atoi(strings.TrimSuffix(lengthText, ":"))
	if err != nil {
		return "", err
	}
	headers := make([]byte, length+1) // the netstring ends with a comma
	if _, err := io.ReadFull(reader, headers); err != nil {
		return "", err
	}
	fields := strings.Split(string(headers[:length]), "\x00")
	if len(fields) < 2 || fields[0] != "CONTENT_LENGTH" {
		return "", fmt.Errorf("SCGI headers must start with CONTENT_LENGTH: %q", fields)
	}
	size, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", err
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return "", err
	}
	return string(body), nil
}

func xmlrpcResponse(value string) string {
	return "Status: 200 OK\r\nContent-Type: text/xml\r\n\r\n" +
		`<?xml version="1.0"?><methodResponse><params><param><value>` + value +
		`</value></param></params></methodResponse>`
}

func xmlrpcArray(values ...string) string {
	var body strings.Builder
	body.WriteString("<array><data>")
	for _, value := range values {
		body.WriteString("<value>" + value + "</value>")
	}
	body.WriteString("</data></array>")
	return body.String()
}

// multicallResults wraps each of count identical results the way
// system.multicall does.
func multicallResults(count int, value string) string {
	results := make([]string, count)
	for i := range results {
		results[i] = xmlrpcArray(value)
	}
	return xmlrpcResponse(xmlrpcArray(results...))
}

func xmlrpcString(text string) string {
	var escaped strings.Builder
	_ = xml.EscapeText(&escaped, []byte(text))
	return "<string>" + escaped.String() + "</string>"
}

func xmlrpcInt(n int) string { return fmt.Sprintf("<i8>%d</i8>", n) }

func TestTelegramUploadReachesRtorrentAsRawBytesWithoutToken(t *testing.T) {
	const token = "123:SUPERSECRET"
	file := []byte("d4:infod4:name4:testee")
	telegramFake := &fakeTelegram{file: file, sent: make(chan sentMessage, 4)}
	rtorrentFake, client := newFakeRtorrent(t)
	app := &application{
		bot:        newTestBot(t, telegramFake, token),
		httpClient: telegramFake,
		rtorrent:   client,
		logger:     log.New(io.Discard, "", 0),
		token:      token,
	}
	message := &models.Message{
		Chat:     models.Chat{ID: 111, Type: models.ChatTypePrivate},
		Document: &models.Document{FileID: "file", FileName: "a.torrent", FileSize: int64(len(file))},
	}
	app.receiveTorrent(context.Background(), 111, message, "d=/remote/path linux")
	expectSent(t, telegramFake.sent, 111, "Added: a.torrent")

	loads := rtorrentFake.requestsContaining("load.raw_start")
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
			erased := len(rtorrentFake.requestsContaining(">d.erase<")) != 0
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
