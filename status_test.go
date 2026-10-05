package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pyed/rtapi"
)

func statusTorrents() rtapi.Torrents {
	hash := func(c string) string { return strings.Repeat(c, 40) }
	return rtapi.Torrents{
		{Name: "getting", Hash: hash("A"), State: rtapi.Leeching, DownRate: 2 << 20},
		{Name: "giving", Hash: hash("B"), State: rtapi.Seeding, UpRate: 1 << 20},
		{Name: "resting", Hash: hash("C"), State: rtapi.Seeding},
		{Name: "broken", Hash: hash("D"), State: rtapi.Error, Message: "Tracker: [Timeout was reached]"},
	}
}

func statusApp(t *testing.T) (*application, *fakeTelegram, *fakeRtorrent) {
	t.Helper()
	app, telegramFake, rtorrentFake := buttonApp(t, statusTorrents())
	app.now = func() time.Time { return time.Date(2026, 10, 5, 14, 5, 0, 0, time.Local) }
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.downRate, f.upRate = 2<<20, 1<<20
		f.limits = [2]uint64{10 << 20, 0}
		f.freeSpace = 1 << 40
		f.peers = map[string][]bool{strings.Repeat("A", 40): {true, false}, strings.Repeat("B", 40): {false}}
	})
	return app, telegramFake, rtorrentFake
}

func statusOf(t *testing.T, app *application) statusMessage {
	t.Helper()
	var status statusMessage
	app.state.read(func(data *stateData) { status = data.Status[111] })
	return status
}

// /status sends a message like ruTorrent's status bar, pins it quietly, and
// keeps it up to date every minute and when 🔄 is tapped.
func TestStatusIsPinnedAndKeptUpToDate(t *testing.T) {
	app, telegramFake, rtorrentFake := statusApp(t)
	command(app, "status")
	sent := nextSent(t, telegramFake)
	want := "📊 rTorrent status\n" +
		"↓ 2.0 MiB/s · limit 10.0 MiB/s · 5.0 GiB this session\n" +
		"↑ 1.0 MiB/s · no limit · 3.0 GiB this session\n" +
		"Torrents: 4 · 1 downloading · 1 uploading · 1 with errors\n" +
		"Peers: 1 in, 2 out · port 6890 open ✅\n" +
		"Free space: 1.0 TiB\n" +
		"Updated 14:05"
	if sent.text != want || strings.Join(buttonTexts(sent.buttons), ",") != "🔄 Refresh=sr" {
		t.Fatalf("status = %q %v, want %q", sent.text, buttonTexts(sent.buttons), want)
	}
	if strings.Join(telegramFake.pins, ",") != "pin 111 1 quietly" || statusOf(t, app) != (statusMessage{Message: sent.messageID}) {
		t.Fatalf("pins = %q, status %+v", telegramFake.pins, statusOf(t, app))
	}

	rtorrentFake.set(func(f *fakeRtorrent) {
		f.downRate = 0
		f.peers = map[string][]bool{strings.Repeat("A", 40): {false}}
	})
	app.checkStatus(context.Background(), time.Date(2026, 10, 5, 14, 6, 0, 0, time.Local))
	updated := lastEdit(t, telegramFake)
	if updated.messageID != sent.messageID || !strings.Contains(updated.text, "\n↓ 0 B/s · limit") ||
		!strings.Contains(updated.text, "\nPeers: 0 in, 1 out · port 6890 may be closed ⚠️\n") || !strings.HasSuffix(updated.text, "\nUpdated 14:06") ||
		!hasButton(updated.buttons, "🔄 Refresh") {
		t.Fatalf("updated = %q", updated.text)
	}

	rtorrentFake.set(func(f *fakeRtorrent) { f.peers = nil })
	press(app, master, sent.messageID, "sr")
	if refreshed := lastEdit(t, telegramFake); !strings.Contains(refreshed.text, "\nPeers: none · port 6890\n") || lastAnswer(t, telegramFake).text != "Updated" {
		t.Fatalf("refreshed = %q, %+v", refreshed.text, lastAnswer(t, telegramFake))
	}

	// After a restart, the bot keeps the message up to date, and its button
	// works again.
	app.screens = screenStore{}
	app.checkStatus(context.Background(), time.Date(2026, 10, 5, 14, 7, 0, 0, time.Local))
	press(app, master, sent.messageID, "sr")
	if answer := lastAnswer(t, telegramFake); answer.text != "Updated" {
		t.Fatalf("refresh after a restart = %+v", answer)
	}
}

func TestStatusShowsQuietHoursAndOutages(t *testing.T) {
	app, telegramFake, rtorrentFake := statusApp(t)
	if err := app.state.update(func(data *stateData) {
		data.Quiet = &quietHours{Start: "23:00", End: "07:00"}
	}); err != nil {
		t.Fatal(err)
	}
	command(app, "status")
	if sent := nextSent(t, telegramFake); strings.Contains(sent.text, "Quiet") {
		t.Fatalf("status outside quiet hours = %q", sent.text)
	}
	saved := [2]uint64{0, 0}
	if err := app.state.update(func(data *stateData) { data.Quiet.Saved = &saved }); err != nil {
		t.Fatal(err)
	}
	command(app, "status")
	if sent := nextSent(t, telegramFake); !strings.HasSuffix(sent.text, "\n🌙 Quiet hours until 07:00\nUpdated 14:05") {
		t.Fatalf("status in quiet hours = %q", sent.text)
	}
	rtorrentFake.set(func(f *fakeRtorrent) { f.faults = map[string]bool{"system.pid": true} })
	app.checkStatus(context.Background(), time.Date(2026, 10, 5, 14, 6, 0, 0, time.Local))
	if updated := lastEdit(t, telegramFake); !strings.HasPrefix(updated.text, "📊 rTorrent status\n🔴 rTorrent is not answering: ") ||
		!strings.HasSuffix(updated.text, "\nUpdated 14:06") {
		t.Fatalf("status while rTorrent is down = %q", updated.text)
	}
}

// A new status replaces the chat's earlier one, and /status off ends it.
func TestStatusReplacesAndStops(t *testing.T) {
	app, telegramFake, rtorrentFake := statusApp(t)
	command(app, "status")
	first := nextSent(t, telegramFake)
	command(app, "status")
	second := nextSent(t, telegramFake)
	if moved := lastEdit(t, telegramFake); moved.messageID != first.messageID || moved.text != "📊 This status moved to a newer message." || len(moved.buttons) != 0 {
		t.Fatalf("the first status = %q %v", moved.text, buttonTexts(moved.buttons))
	}
	if got := strings.Join(telegramFake.pins, ","); got != fmt.Sprintf("pin 111 %d quietly,unpin 111 %d,pin 111 %d quietly", first.messageID, first.messageID, second.messageID) ||
		statusOf(t, app).Message != second.messageID {
		t.Fatalf("pins = %q, status %+v", got, statusOf(t, app))
	}
	press(app, master, first.messageID, "sr")
	if answer := lastAnswer(t, telegramFake); !answer.alert {
		t.Fatalf("refresh on the old status = %+v", answer)
	}

	if got := lastSentText(t, telegramFake, app, "status off"); got != "The status message is no longer updated or pinned." {
		t.Fatalf("status off = %q", got)
	}
	if stopped := lastEdit(t, telegramFake); stopped.messageID != second.messageID || !strings.HasPrefix(stopped.text, "📊 This status is no longer updated.") {
		t.Fatalf("the stopped status = %q", stopped.text)
	}
	if statusOf(t, app) != (statusMessage{}) || !strings.HasSuffix(strings.Join(telegramFake.pins, ","), fmt.Sprintf(",unpin 111 %d", second.messageID)) {
		t.Fatalf("after status off: status %+v, pins %q", statusOf(t, app), telegramFake.pins)
	}
	// With no status message, the check asks rTorrent nothing.
	before := len(rtorrentFake.called("d.multicall2"))
	app.checkStatus(context.Background(), time.Now())
	if len(rtorrentFake.called("d.multicall2")) != before {
		t.Fatal("checked rTorrent with no status message")
	}
	for text, want := range map[string]string{"status off": "status: this chat has no status message", "status now": "status: use status, or status off"} {
		if got := lastSentText(t, telegramFake, app, text); got != want {
			t.Errorf("%s = %q, want %q", text, got, want)
		}
	}
}

// A status message someone deleted ends that chat's status.
func TestStatusEndsWhenItsMessageIsGone(t *testing.T) {
	app, telegramFake, _ := statusApp(t)
	command(app, "status")
	sent := nextSent(t, telegramFake)
	telegramFake.editRefused = map[int]string{sent.messageID: "Bad Request: message to edit not found"}
	app.checkStatus(context.Background(), time.Now())
	if statusOf(t, app) != (statusMessage{}) {
		t.Fatalf("status = %+v", statusOf(t, app))
	}
	edits := len(telegramFake.edits)
	app.checkStatus(context.Background(), time.Now())
	if len(telegramFake.edits) != edits {
		t.Fatal("edited a deleted status message")
	}

	// Telegram refuses an edit that changes nothing, which is no failure.
	command(app, "status")
	sent = nextSent(t, telegramFake)
	telegramFake.editRefused = map[int]string{sent.messageID: "Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}
	press(app, master, sent.messageID, "sr")
	if answer := lastAnswer(t, telegramFake); answer.text != "Updated" || answer.alert || statusOf(t, app).Message != sent.messageID {
		t.Fatalf("an unchanged refresh = %+v", answer)
	}

	// A chat whose status ended meanwhile is left alone.
	edits = len(telegramFake.edits)
	if err := app.updateStatus(context.Background(), 222, "text"); err != errNoStatus || len(telegramFake.edits) != edits {
		t.Fatalf("updating a chat without a status = %v", err)
	}

	// Other failures leave it for the next minute.
	command(app, "status")
	sent = nextSent(t, telegramFake)
	telegramFake.editRefused = map[int]string{sent.messageID: "Bad Request: something else"}
	app.checkStatus(context.Background(), time.Now())
	if statusOf(t, app).Message != sent.messageID {
		t.Fatalf("status after another failure = %+v", statusOf(t, app))
	}
}

func TestStatusSaysWhenItCannotBePinned(t *testing.T) {
	app, telegramFake, _ := statusApp(t)
	telegramFake.pinRefused = map[int64]bool{111: true}
	command(app, "status")
	nextSent(t, telegramFake)
	if got := nextSent(t, telegramFake).text; !strings.HasPrefix(got, "status: it stays up to date, but could not be pinned: ") ||
		!strings.HasSuffix(got, "In a group, the bot needs the right to pin messages.") {
		t.Fatalf("reply = %q", got)
	}
	if statusOf(t, app).Message == 0 {
		t.Fatal("an unpinned status is not kept up to date")
	}
}
