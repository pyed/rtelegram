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

type stubTelegramBot struct {
	sent    []tgbotapi.Chattable
	updates chan tgbotapi.Update
	files   map[string]tgbotapi.File
}

func newStubTelegramBot() *stubTelegramBot {
	return &stubTelegramBot{
		updates: make(chan tgbotapi.Update),
		files:   make(map[string]tgbotapi.File),
	}
}

func (b *stubTelegramBot) Send(payload tgbotapi.Chattable) (tgbotapi.Message, error) {
	b.sent = append(b.sent, payload)
	return tgbotapi.Message{MessageID: len(b.sent)}, nil
}

func (b *stubTelegramBot) GetUpdatesChan(tgbotapi.UpdateConfig) (<-chan tgbotapi.Update, error) {
	return b.updates, nil
}

func (b *stubTelegramBot) GetFile(cfg tgbotapi.FileConfig) (tgbotapi.File, error) {
	if file, ok := b.files[cfg.FileID]; ok {
		return file, nil
	}
	return tgbotapi.File{FileID: cfg.FileID, FilePath: cfg.FileID}, nil
}

func (b *stubTelegramBot) sentMessages() []tgbotapi.MessageConfig {
	var msgs []tgbotapi.MessageConfig
	for _, payload := range b.sent {
		if msg, ok := payload.(tgbotapi.MessageConfig); ok {
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

func (b *stubTelegramBot) sentEdits() []tgbotapi.EditMessageTextConfig {
	var edits []tgbotapi.EditMessageTextConfig
	for _, payload := range b.sent {
		if edit, ok := payload.(tgbotapi.EditMessageTextConfig); ok {
			edits = append(edits, edit)
		}
	}
	return edits
}


func (b *stubTelegramBot) sentActions() []tgbotapi.ChatActionConfig {
	var actions []tgbotapi.ChatActionConfig
	for _, payload := range b.sent {
		if action, ok := payload.(tgbotapi.ChatActionConfig); ok {
			actions = append(actions, action)
		}
	}
	return actions
}

type stubRtorrent struct {
	torrents rtapi.Torrents
	stats    rtapi.Stats
	down     uint64
	up       uint64
	version  string
}

func newStubRtorrent() *stubRtorrent {
	return &stubRtorrent{version: "test"}
}

func (r *stubRtorrent) Torrents() (rtapi.Torrents, error)    { return r.torrents, nil }
func (r *stubRtorrent) Start(...*rtapi.Torrent) error        { return nil }
func (r *stubRtorrent) Stop(...*rtapi.Torrent) error         { return nil }
func (r *stubRtorrent) Check(...*rtapi.Torrent) error        { return nil }
func (r *stubRtorrent) Delete(bool, ...*rtapi.Torrent) error { return nil }
func (r *stubRtorrent) Download(string) error                { return nil }
func (r *stubRtorrent) DownloadWithOptions(*rtapi.DotTorrentWithOptions) error {
	return nil
}
func (r *stubRtorrent) Stats() (*rtapi.Stats, error) { return &r.stats, nil }
func (r *stubRtorrent) Speeds() (uint64, uint64)     { return r.down, r.up }
func (r *stubRtorrent) GetTorrent(hash string) (*rtapi.Torrent, error) {
	for _, torrent := range r.torrents {
		if torrent.Hash == hash {
			return torrent, nil
		}
	}
	return &rtapi.Torrent{}, nil
}
func (r *stubRtorrent) Version() string { return r.version }

func (r *stubRtorrent) setTorrents(torrents ...*rtapi.Torrent) {
	r.torrents = torrents
}

func (r *stubRtorrent) setStats(stats rtapi.Stats) {
	r.stats = stats
}

func (r *stubRtorrent) setSpeeds(down, up uint64) {
	r.down, r.up = down, up
}

func setupTestEnvironment(t *testing.T) (*stubTelegramBot, *stubRtorrent) {
	t.Helper()

	prevDuration, prevInterval := duration, interval
	duration = 0
	interval = 0
	t.Cleanup(func() {
		duration = prevDuration
		interval = prevInterval
	})

	bot := newStubTelegramBot()
	Bot = bot
	botUsername = "test-bot"
	Updates = bot.updates
	chatID = 1

	rt := newStubRtorrent()
	rtorrent = rt

	return bot, rt
}

func newTorrent(name string, state rtapi.State) *rtapi.Torrent {
	return &rtapi.Torrent{
		Name:      name,
		State:     state,
		Percent:   "50%",
		Completed: 2048,
		DownRate:  0,
		UpRate:    0,
		UpTotal:   4096,
		Ratio:     rtapi.Ratio{Value: 1.5},
		Age:       1700000000,
		ETA:       60,
		Tracker:   &rtapi.Tracker{},
		Hash:      name + "-hash",
	}
}

func TestRunningInTest(t *testing.T) {
	if !runningInTest() {
		t.Fatal("runningInTest should return true during tests")
	}
}

func TestSendSplitsLongMessages(t *testing.T) {
	bot, _ := setupTestEnvironment(t)

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

	if len(bot.sentActions()) != 1 {
		t.Fatalf("expected one chat action, got %d", len(bot.sentActions()))
	}
	msgs := bot.sentMessages()
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

func TestListNoTorrents(t *testing.T) {
	bot, _ := setupTestEnvironment(t)

	list(nil)

	msgs := bot.sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if msgs[0].Text != "list: No torrents" {
		t.Fatalf("unexpected message text: %q", msgs[0].Text)
	}
}

func TestListWithTorrents(t *testing.T) {
	bot, rt := setupTestEnvironment(t)

	rt.setTorrents(newTorrent("alpha", rtapi.Seeding), newTorrent("beta", rtapi.Leeching))

	list(nil)

	msgs := bot.sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "<0> alpha") || !strings.Contains(msgs[0].Text, "<1> beta") {
		t.Fatalf("unexpected list output: %q", msgs[0].Text)
	}
}

func TestActiveFiltersBySpeed(t *testing.T) {
	bot, rt := setupTestEnvironment(t)

	torrents := []*rtapi.Torrent{
		newTorrent("idle", rtapi.Stopped),
		newTorrent("downloading", rtapi.Leeching),
		newTorrent("seeding", rtapi.Seeding),
	}
	torrents[1].DownRate = 2048
	torrents[2].UpRate = 1024
	rt.setTorrents(torrents...)

	active()

	msgs := bot.sentMessages()
	if len(msgs) == 0 {
		t.Fatal("expected active to send at least one message")
	}
	text := msgs[0].Text
	if strings.Contains(text, "idle") {
		t.Fatalf("idle torrent should not appear in active output: %q", text)
	}
	if !strings.Contains(text, "downloading") || !strings.Contains(text, "seeding") {
		t.Fatalf("expected active output to include seeding and downloading torrents: %q", text)
	}
}

func TestStatsFormatting(t *testing.T) {
	bot, rt := setupTestEnvironment(t)

	rt.setStats(rtapi.Stats{
		ThrottleUp:   0,
		ThrottleDown: 1024,
		Port:         "5000",
		Directory:    "/torrents",
		TotalUp:      4096,
		TotalDown:    8192,
	})

	stats()

	msgs := bot.sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one stats message, got %d", len(msgs))
	}
	text := msgs[0].Text
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "throttle  *off* / *1.0 kb*") {
		t.Fatalf("expected throttling details in stats output: %q", text)
	}
	if !strings.Contains(lower, "total uploaded: *4.0 kb*") || !strings.Contains(lower, "total download: *8.0 kb*") {
		t.Fatalf("expected totals in stats output: %q", text)
	}
}

func TestSpeedReporting(t *testing.T) {
	bot, rt := setupTestEnvironment(t)

	rt.setSpeeds(2048, 4096)

	speed()

	msgs := bot.sentMessages()
	if len(msgs) == 0 {
		t.Fatal("expected speed to send a message")
	}
	if want := "↓ 2.0 kb  ↑ 4.0 kb"; strings.ToLower(msgs[0].Text) != want {
		t.Fatalf("unexpected speed output: got %q want %q", msgs[0].Text, "↓ 2.0 kB  ↑ 4.0 kB")
	}
}

func TestVersionCommand(t *testing.T) {
	bot, rt := setupTestEnvironment(t)

	rt.version = "9.9"

	getVersion()

	msgs := bot.sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one version message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "9.9") || !strings.Contains(msgs[0].Text, VERSION) {
		t.Fatalf("version output missing expected values: %q", msgs[0].Text)
	}
}
