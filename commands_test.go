package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/pyed/rtapi"
	tgbotapi "gopkg.in/telegram-bot-api.v4"
)

func setupTestEnvironment(t *testing.T) {
	t.Helper()
	resetBot(t)
	resetRtorrent(t)
	prevDuration, prevInterval := duration, interval
	duration = 0
	interval = 0
	t.Cleanup(func() {
		duration = prevDuration
		interval = prevInterval
	})
}

func resetBot(t *testing.T) {
	t.Helper()
	tgbotapi.SentPayloads = nil
	bot, err := tgbotapi.NewBotAPI("test-token")
	if err != nil {
		t.Fatalf("failed to init bot: %v", err)
	}
	Bot = bot
	chatID = 1
}

func resetRtorrent(t *testing.T) {
	t.Helper()
	client, err := rtapi.NewRtorrent(SCGIURL)
	if err != nil {
		t.Fatalf("failed to init rtorrent: %v", err)
	}
	rtorrent = client
}

func setTorrents(t *testing.T, torrents ...*rtapi.Torrent) {
	t.Helper()
	v := reflect.ValueOf(rtorrent).Elem().FieldByName("torrents")
	reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Set(reflect.ValueOf(rtapi.Torrents(torrents)))
}

func setStats(t *testing.T, stats rtapi.Stats) {
	t.Helper()
	v := reflect.ValueOf(rtorrent).Elem().FieldByName("stats")
	reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Set(reflect.ValueOf(stats))
}

func setSpeeds(t *testing.T, down, up uint64) {
	t.Helper()
	speedsField := reflect.ValueOf(rtorrent).Elem().FieldByName("speeds")
	speeds := reflect.NewAt(speedsField.Type(), unsafe.Pointer(speedsField.UnsafeAddr())).Elem()
	downField := speeds.FieldByName("down")
	reflect.NewAt(downField.Type(), unsafe.Pointer(downField.UnsafeAddr())).Elem().SetUint(down)
	upField := speeds.FieldByName("up")
	reflect.NewAt(upField.Type(), unsafe.Pointer(upField.UnsafeAddr())).Elem().SetUint(up)
}

func newTracker(host string) *rtapi.Tracker {
	tracker := &rtapi.Tracker{}
	v := reflect.ValueOf(tracker).Elem().FieldByName("host")
	reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().SetString(host)
	return tracker
}

func newTorrent(name string, state rtapi.State, tracker string) *rtapi.Torrent {
	return &rtapi.Torrent{
		Name:      name,
		State:     state,
		Percent:   "50%",
		Completed: 2048,
		DownRate:  1024,
		UpRate:    512,
		UpTotal:   4096,
		Ratio:     rtapi.Ratio{Value: 1.5},
		Age:       1700000000,
		ETA:       60,
		Message:   "error message",
		Tracker:   newTracker(tracker),
		Hash:      name + "-hash",
	}
}

func sentMessages() []tgbotapi.MessageConfig {
	var msgs []tgbotapi.MessageConfig
	for _, payload := range tgbotapi.SentPayloads {
		if msg, ok := payload.(tgbotapi.MessageConfig); ok {
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

func sentEdits() []tgbotapi.EditMessageTextConfig {
	var edits []tgbotapi.EditMessageTextConfig
	for _, payload := range tgbotapi.SentPayloads {
		if msg, ok := payload.(tgbotapi.EditMessageTextConfig); ok {
			edits = append(edits, msg)
		}
	}
	return edits
}

func sentActions() []tgbotapi.ChatActionConfig {
	var actions []tgbotapi.ChatActionConfig
	for _, payload := range tgbotapi.SentPayloads {
		if action, ok := payload.(tgbotapi.ChatActionConfig); ok {
			actions = append(actions, action)
		}
	}
	return actions
}

func TestRunningInTest(t *testing.T) {
	if !runningInTest() {
		t.Fatal("runningInTest should return true during tests")
	}
}

func TestSendSplitsLongMessages(t *testing.T) {
	setupTestEnvironment(t)

	var builder strings.Builder
	for i := 0; i < 200; i++ {
		builder.WriteString(strings.Repeat("x", 50))
		builder.WriteByte('\n')
	}
	original := builder.String()
	if len(original) <= 4096 {
		t.Fatalf("expected message >4096 bytes, got %d", len(original))
	}

	send(original, false)

	if len(sentActions()) != 1 {
		t.Fatalf("expected one chat action, got %d", len(sentActions()))
	}
	msgs := sentMessages()
	if len(msgs) < 2 {
		t.Fatalf("expected message to be split into multiple payloads, got %d", len(msgs))
	}

	var combined strings.Builder
	for _, msg := range msgs {
		combined.WriteString(msg.Text)
	}
	if combined.String() != original {
		t.Fatalf("combined message does not match original text")
	}
}

func TestSortModes(t *testing.T) {
	setupTestEnvironment(t)

	tests := []struct {
		name     string
		tokens   []string
		expected rtapi.SortMode
		message  string
	}{
		{name: "default help", tokens: nil, expected: rtapi.SortMode(0), message: "sort takes one of:"},
		{name: "by name", tokens: []string{"name"}, expected: rtapi.ByName, message: "sort: by `name`"},
		{name: "by name reversed", tokens: []string{"rev", "name"}, expected: rtapi.ByNameRev, message: "sort: by `reversed name`"},
		{name: "unknown", tokens: []string{"unknown"}, expected: rtapi.ByNameRev, message: "unkown sorting method"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetBot(t)
			if tt.name == "default help" {
				// reset sorting to a known value
				rtapi.CurrentSorting = rtapi.ByName
			}
			sort(tt.tokens)
			msgs := sentMessages()
			if len(msgs) == 0 {
				t.Fatalf("expected a message for %s", tt.name)
			}
			last := msgs[len(msgs)-1].Text
			if !strings.Contains(last, tt.message) {
				t.Fatalf("message %q does not contain %q", last, tt.message)
			}
			if len(tt.tokens) == 0 {
				return
			}
			if tt.name != "unknown" && rtapi.CurrentSorting != tt.expected {
				t.Fatalf("sorting was %v, expected %v", rtapi.CurrentSorting, tt.expected)
			}
		})
	}
}

func TestListVariants(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("Alpha", rtapi.Seeding, "tracker.alpha"),
		newTorrent("Beta", rtapi.Leeching, "tracker.beta"),
	}
	setTorrents(t, torrents...)

	t.Run("all", func(t *testing.T) {
		resetBot(t)
		list(nil)
		msgs := sentMessages()
		if len(msgs) != 1 {
			t.Fatalf("expected one message, got %d", len(msgs))
		}
		expected := "<0> Alpha\n<1> Beta\n"
		if msgs[0].Text != expected {
			t.Fatalf("got %q want %q", msgs[0].Text, expected)
		}
	})

	t.Run("filter", func(t *testing.T) {
		resetBot(t)
		list([]string{"alpha"})
		msgs := sentMessages()
		if len(msgs) != 1 {
			t.Fatalf("expected one message, got %d", len(msgs))
		}
		expected := "<0> Alpha\n"
		if msgs[0].Text != expected {
			t.Fatalf("got %q want %q", msgs[0].Text, expected)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		resetBot(t)
		list([]string{"gamma"})
		msgs := sentMessages()
		if len(msgs) != 1 {
			t.Fatalf("expected one message, got %d", len(msgs))
		}
		expected := "list: No tracker matches: *gamma*"
		if msgs[0].Text != expected {
			t.Fatalf("got %q want %q", msgs[0].Text, expected)
		}
	})

	t.Run("invalid regex", func(t *testing.T) {
		resetBot(t)
		list([]string{"["})
		msgs := sentMessages()
		if len(msgs) != 1 {
			t.Fatalf("expected one message, got %d", len(msgs))
		}
		expected := "list: error parsing regexp: missing closing ]: `[`"
		if msgs[0].Text != expected {
			t.Fatalf("unexpected message: %q", msgs[0].Text)
		}
	})
}

func TestDownsAndSeeding(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("Leecher", rtapi.Leeching, "tracker.a"),
		newTorrent("Seeder", rtapi.Seeding, "tracker.b"),
	}
	setTorrents(t, torrents...)

	resetBot(t)
	downs()
	msgs := sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if msgs[0].Text != "<0> Leecher\n" {
		t.Fatalf("unexpected downs output: %q", msgs[0].Text)
	}

	resetBot(t)
	seeding()
	msgs = sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if msgs[0].Text != "<1> Seeder\n" {
		t.Fatalf("unexpected seeding output: %q", msgs[0].Text)
	}

	resetBot(t)
	setTorrents(t)
	downs()
	if sentMessages()[0].Text != "No downloads" {
		t.Fatalf("expected no downloads message, got %q", sentMessages()[0].Text)
	}
}

func TestPausedAndHashing(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("Stopped", rtapi.Stopped, "tracker"),
		newTorrent("Hashing", rtapi.Hashing, "tracker"),
	}
	torrents[0].UpTotal = 8192
	setTorrents(t, torrents...)

	resetBot(t)
	paused()
	msgs := sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected paused output, got %d messages", len(msgs))
	}
	expectedPaused := "<0> Stopped\nStopped (50%) DL: 2048 B UL: 8192 B  R: 1.50\n\n"
	if msgs[0].Text != expectedPaused {
		t.Fatalf("unexpected paused output: %q", msgs[0].Text)
	}

	resetBot(t)
	hashing()
	msgs = sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected hashing output, got %d messages", len(msgs))
	}
	expectedHashing := "<1> Hashing\nHashing (50%)\n\n"
	if msgs[0].Text != expectedHashing {
		t.Fatalf("unexpected hashing output: %q", msgs[0].Text)
	}
}

func TestErrors(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("Good", rtapi.Seeding, "tracker"),
		newTorrent("Bad", rtapi.Error, "tracker"),
	}
	torrents[1].Message = "disk full"
	setTorrents(t, torrents...)

	resetBot(t)
	errors()
	msgs := sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected error output, got %d messages", len(msgs))
	}
	expected := "<1> Bad\ndisk full\n\n"
	if msgs[0].Text != expected {
		t.Fatalf("unexpected errors output: %q", msgs[0].Text)
	}
}

func TestLatest(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("Newest", rtapi.Seeding, "tracker"),
		newTorrent("Older", rtapi.Seeding, "tracker"),
	}
	setTorrents(t, torrents...)

	resetBot(t)
	latest([]string{"1"})
	msgs := sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if msgs[0].Text != "<0> Newest\n" {
		t.Fatalf("unexpected latest output: %q", msgs[0].Text)
	}

	resetBot(t)
	latest([]string{"bad"})
	if sentMessages()[0].Text != "latest: argument must be a number" {
		t.Fatalf("expected argument error message")
	}

	resetBot(t)
	setTorrents(t)
	latest(nil)
	if sentMessages()[0].Text != "latest: No torrents" {
		t.Fatalf("expected no torrents message")
	}
}

func TestCount(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("Leecher", rtapi.Leeching, "tracker"),
		newTorrent("Seeder", rtapi.Seeding, "tracker"),
		newTorrent("Complete", rtapi.Complete, "tracker"),
		newTorrent("Stopped", rtapi.Stopped, "tracker"),
		newTorrent("Hasher", rtapi.Hashing, "tracker"),
		newTorrent("Error", rtapi.Error, "tracker"),
	}
	setTorrents(t, torrents...)

	resetBot(t)
	count()
	msgs := sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected single message, got %d", len(msgs))
	}
	expected := "Leeching: *1*\nSeeding: *1*\nComplete: *1*\nStopped: *1*\nHashing: *1*\nError: *1*\n\nTotal: *6*"
	if msgs[0].Text != expected {
		t.Fatalf("unexpected count output: %q", msgs[0].Text)
	}
}

func TestSearch(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("Alpha Release", rtapi.Seeding, "tracker"),
		newTorrent("Beta", rtapi.Seeding, "tracker"),
	}
	setTorrents(t, torrents...)

	resetBot(t)
	search([]string{"alpha"})
	if sentMessages()[0].Text != "<0> Alpha Release\n" {
		t.Fatalf("unexpected search output: %q", sentMessages()[0].Text)
	}

	resetBot(t)
	search([]string{})
	if sentMessages()[0].Text != "search: needs an argument" {
		t.Fatalf("expected missing argument message")
	}

	resetBot(t)
	search([]string{"["})
	expected := "search: error parsing regexp: missing closing ]: `[`"
	if sentMessages()[0].Text != expected {
		t.Fatalf("expected regex error message, got %q", sentMessages()[0].Text)
	}
}

func TestStats(t *testing.T) {
	setupTestEnvironment(t)
	statsData := rtapi.Stats{
		ThrottleUp:   0,
		ThrottleDown: 1024,
		Port:         "5000",
		Directory:    "/data",
		TotalUp:      4096,
		TotalDown:    8192,
	}
	setStats(t, statsData)

	resetBot(t)
	stats()
	msgs := sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected stats output, got %d messages", len(msgs))
	}
	trimmed := strings.TrimSpace(msgs[0].Text)
	expected := "\\[Throttle  *off* / *1024 B*]\n\\[Port *5000*]\n\\[*/data*]\nTotal Uploaded: *4096 B*\nTotal Download: *8192 B*"
	if trimmed != expected {
		t.Fatalf("unexpected stats output: %q", trimmed)
	}
}

func TestSpeed(t *testing.T) {
	setupTestEnvironment(t)
	setSpeeds(t, 2048, 1024)

	resetBot(t)
	speed()
	msgs := sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one speed message, got %d", len(msgs))
	}
	if msgs[0].Text != "↓ 2048 B  ↑ 1024 B" {
		t.Fatalf("unexpected speed message: %q", msgs[0].Text)
	}
	edits := sentEdits()
	if len(edits) == 0 {
		t.Fatalf("expected an edit message at the end")
	}
	if edits[len(edits)-1].Text != "↓ - B  ↑ - B" {
		t.Fatalf("unexpected final edit text: %q", edits[len(edits)-1].Text)
	}
}

func TestGetVersion(t *testing.T) {
	setupTestEnvironment(t)
	rtorrent.Version = "3.10"

	resetBot(t)
	getVersion()
	msgs := sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected version message, got %d", len(msgs))
	}
	expected := "rTorrent/libtorrent: *3.10*\nrtelegram: *" + VERSION + "*"
	if msgs[0].Text != expected {
		t.Fatalf("unexpected version output: %q", msgs[0].Text)
	}
}

func TestAddAndDeleteCommands(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("Sample", rtapi.Seeding, "tracker"),
	}
	setTorrents(t, torrents...)

	resetBot(t)
	add([]string{}, "")
	if sentMessages()[0].Text != "add: needs at least one URL" {
		t.Fatalf("expected add argument error")
	}

	resetBot(t)
	add([]string{"http://example.com/sample.torrent"}, "")
	if sentMessages()[0].Text != "Added: sample.torrent" {
		t.Fatalf("unexpected add output: %q", sentMessages()[0].Text)
	}

	resetBot(t)
	del([]string{})
	if sentMessages()[0].Text != "del: needs an ID" {
		t.Fatalf("expected del argument error")
	}

	resetBot(t)
	del([]string{"x"})
	if sentMessages()[0].Text != "del: x is not an ID" {
		t.Fatalf("expected del invalid id message")
	}

	resetBot(t)
	del([]string{"0"})
	if sentMessages()[0].Text != "Deleted: Sample" {
		t.Fatalf("unexpected del output: %q", sentMessages()[0].Text)
	}

	resetBot(t)
	deldata([]string{})
	if sentMessages()[0].Text != "deldata: needs an ID" {
		t.Fatalf("expected deldata argument error")
	}

	resetBot(t)
	deldata([]string{"0"})
	if sentMessages()[0].Text != "Deleted with data: Sample" {
		t.Fatalf("unexpected deldata output: %q", sentMessages()[0].Text)
	}
}

func TestStartStopAndCheck(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("Alpha", rtapi.Seeding, "tracker"),
		newTorrent("Beta", rtapi.Seeding, "tracker"),
	}
	setTorrents(t, torrents...)

	resetBot(t)
	start([]string{})
	if sentMessages()[0].Text != "start: needs an argument" {
		t.Fatalf("expected start argument error")
	}

	resetBot(t)
	start([]string{"all"})
	if sentMessages()[0].Text != "started all torrents" {
		t.Fatalf("unexpected start all output: %q", sentMessages()[0].Text)
	}

	resetBot(t)
	start([]string{"1"})
	if sentMessages()[0].Text != "Started: Beta" {
		t.Fatalf("unexpected start output: %q", sentMessages()[0].Text)
	}

	resetBot(t)
	start([]string{"bad"})
	if sentMessages()[0].Text != "start: bad is not a number" {
		t.Fatalf("expected start invalid number message")
	}

	resetBot(t)
	stop([]string{"all"})
	if sentMessages()[0].Text != "stopped all torrents" {
		t.Fatalf("unexpected stop all output: %q", sentMessages()[0].Text)
	}

	resetBot(t)
	stop([]string{"0"})
	if sentMessages()[0].Text != "Stopped: Alpha" {
		t.Fatalf("unexpected stop output: %q", sentMessages()[0].Text)
	}

	resetBot(t)
	check([]string{})
	if sentMessages()[0].Text != "check: needs an argument" {
		t.Fatalf("expected check argument error")
	}

	resetBot(t)
	check([]string{"all"})
	if sentMessages()[0].Text != "hash checking all torrents" {
		t.Fatalf("unexpected check all output: %q", sentMessages()[0].Text)
	}

	resetBot(t)
	check([]string{"1"})
	if sentMessages()[0].Text != "Checking: Beta" {
		t.Fatalf("unexpected check single output: %q", sentMessages()[0].Text)
	}
}

func TestTrackers(t *testing.T) {
	setupTestEnvironment(t)
	torrents := []*rtapi.Torrent{
		newTorrent("One", rtapi.Seeding, "tracker.one"),
		newTorrent("Two", rtapi.Seeding, "tracker.one"),
		newTorrent("Three", rtapi.Seeding, "tracker.two"),
	}
	setTorrents(t, torrents...)

	resetBot(t)
	trackers()
	msgs := sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected trackers output")
	}
	text := msgs[0].Text
	if !strings.Contains(text, "2 - tracker.one") || !strings.Contains(text, "1 - tracker.two") {
		t.Fatalf("unexpected trackers output: %q", text)
	}
}

func TestReceiveTorrent(t *testing.T) {
	setupTestEnvironment(t)
	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "downloads")
	update := tgbotapi.Update{
		Message: &tgbotapi.Message{
			Document: &tgbotapi.Document{FileID: "file-id", FileName: "sample.torrent"},
			Caption:  "d=" + targetDir + " l=Movies",
		},
	}

	resetBot(t)
	receiveTorrent(update)

	if _, err := os.Stat(targetDir); err != nil {
		t.Fatalf("expected directory to be created: %v", err)
	}

	msgs := sentMessages()
	if len(msgs) < 1 {
		t.Fatalf("expected at least one message")
	}
	if msgs[0].Text != "New directory created: "+targetDir {
		t.Fatalf("unexpected directory creation message: %q", msgs[0].Text)
	}
	if msgs[len(msgs)-1].Text != "Added: sample.torrent" {
		t.Fatalf("expected final add message, got %q", msgs[len(msgs)-1].Text)
	}

	resetBot(t)
	receiveTorrent(tgbotapi.Update{Message: &tgbotapi.Message{}})
	if len(sentMessages()) != 0 {
		t.Fatalf("expected no messages for update without document")
	}
}
