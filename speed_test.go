package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseLimits(t *testing.T) {
	value := func(rate uint64) *uint64 { return &rate }
	tests := []struct {
		text     string
		down, up *uint64
	}{
		{"down 5M up 1M", value(5 << 20), value(1 << 20)},
		{"up 512K", nil, value(512 << 10)},
		{"d 0 u off", value(0), value(0)},
		{"DOWN unlimited", value(0), nil},
	}
	for _, test := range tests {
		down, up, err := parseLimits(strings.Fields(test.text))
		if err != nil || !equalRates(down, test.down) || !equalRates(up, test.up) {
			t.Errorf("parseLimits(%q) = %v, %v, %v", test.text, down, up, err)
		}
	}
	for _, text := range []string{"", "down", "sideways 5M", "down fast", "down 5M up"} {
		if _, _, err := parseLimits(strings.Fields(text)); err == nil {
			t.Errorf("parseLimits(%q) succeeded", text)
		}
	}
}

func equalRates(x, y *uint64) bool {
	return (x == nil && y == nil) || (x != nil && y != nil && *x == *y)
}

func TestInWindow(t *testing.T) {
	at := func(clock string) time.Time {
		parsed, _ := time.Parse("15:04", clock)
		return time.Date(2026, 10, 1, parsed.Hour(), parsed.Minute(), 0, 0, time.Local)
	}
	for _, test := range []struct {
		start, end, now string
		inside          bool
	}{
		{"23:00", "07:00", "22:59", false},
		{"23:00", "07:00", "23:00", true},
		{"23:00", "07:00", "03:00", true},
		{"23:00", "07:00", "07:00", false},
		{"09:00", "17:00", "12:30", true},
		{"09:00", "17:00", "08:59", false},
		{"09:00", "17:00", "17:00", false},
	} {
		if got := inWindow(test.start, test.end, at(test.now)); got != test.inside {
			t.Errorf("inWindow(%s-%s, %s) = %v", test.start, test.end, test.now, got)
		}
	}
}

func limitsOf(rtorrentFake *fakeRtorrent) [2]uint64 {
	var limits [2]uint64
	rtorrentFake.set(func(f *fakeRtorrent) { limits = f.limits })
	return limits
}

func TestLimitCommandSetsAndShows(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, nil)

	command(app, "limit")
	screen := nextSent(t, telegramFake)
	if !strings.HasPrefix(screen.text, "Speed limits\n↓ unlimited ↑ unlimited") || !hasButton(screen.buttons, "↓ 5M") || !hasButton(screen.buttons, "↑ 256K") {
		t.Fatalf("limit screen = %q %v", screen.text, buttonTexts(screen.buttons))
	}

	command(app, "limit down 5M up 1M")
	if got := nextSent(t, telegramFake).text; limitsOf(rtorrentFake) != [2]uint64{5 << 20, 1 << 20} || !strings.Contains(got, "↓ 5.0 MiB/s ↑ 1.0 MiB/s") {
		t.Fatalf("limits = %v, reply %q", limitsOf(rtorrentFake), got)
	}
	lastSentText(t, telegramFake, app, "limit up 512K")
	if limitsOf(rtorrentFake) != [2]uint64{5 << 20, 512 << 10} {
		t.Fatalf("setting up changed down: %v", limitsOf(rtorrentFake))
	}
	if got := lastSentText(t, telegramFake, app, "limit sideways 5M"); got != "limit: sideways is not down or up" {
		t.Fatalf("bad direction = %q", got)
	}

	press(app, master, screen.messageID, "lm:d:1048576")
	if limitsOf(rtorrentFake) != [2]uint64{1 << 20, 512 << 10} || !strings.Contains(lastEdit(t, telegramFake).text, "↓ 1.0 MiB/s") {
		t.Fatalf("after preset: %v", limitsOf(rtorrentFake))
	}
	press(app, master, screen.messageID, "lm:off")
	command(app, "limit off")
	if limitsOf(rtorrentFake) != [2]uint64{0, 0} {
		t.Fatalf("after removing limits: %v", limitsOf(rtorrentFake))
	}
}

// quietApp is an app whose clock reads clock, with limits of 10M down, 2M up.
func quietApp(t *testing.T, clock *time.Time) (*application, *fakeTelegram, *fakeRtorrent) {
	t.Helper()
	app, telegramFake, rtorrentFake := buttonApp(t, nil)
	app.now = func() time.Time { return *clock }
	rtorrentFake.set(func(f *fakeRtorrent) { f.limits = [2]uint64{10 << 20, 2 << 20} })
	return app, telegramFake, rtorrentFake
}

func at(hour, minute int) time.Time {
	return time.Date(2026, 10, 1, hour, minute, 0, 0, time.Local)
}

func TestQuietHoursApplyAndRestore(t *testing.T) {
	clock := at(22, 30)
	app, telegramFake, rtorrentFake := quietApp(t, &clock)
	ctx := context.Background()

	if got := lastSentText(t, telegramFake, app, "quiet 23:00-07:00 down 2M"); !strings.HasPrefix(got, "Quiet hours 23:00–07:00: ↓ 2.0 MiB/s\n") {
		t.Fatalf("reply = %q", got)
	}
	if limitsOf(rtorrentFake) != [2]uint64{10 << 20, 2 << 20} {
		t.Fatal("quiet limits applied before the window")
	}
	app.checkQuiet(ctx, at(23, 0))
	if limitsOf(rtorrentFake) != [2]uint64{2 << 20, 2 << 20} {
		t.Fatalf("at 23:00 limits = %v", limitsOf(rtorrentFake))
	}
	app.checkQuiet(ctx, at(3, 0))
	if !strings.HasSuffix(app.quietSummary(), "(on now)") {
		t.Fatalf("summary during quiet hours = %q", app.quietSummary())
	}
	app.checkQuiet(ctx, at(7, 0))
	if limitsOf(rtorrentFake) != [2]uint64{10 << 20, 2 << 20} {
		t.Fatalf("at 07:00 limits = %v", limitsOf(rtorrentFake))
	}
	app.state.read(func(data *stateData) {
		if data.Quiet == nil || data.Quiet.Saved != nil {
			t.Fatalf("quiet state after the window = %+v", data.Quiet)
		}
	})
}

func TestLimitChangedDuringQuietHoursIsRestored(t *testing.T) {
	clock := at(1, 0)
	app, telegramFake, rtorrentFake := quietApp(t, &clock)
	lastSentText(t, telegramFake, app, "quiet 23:00-07:00 down 2M")
	if limitsOf(rtorrentFake)[0] != 2<<20 {
		t.Fatalf("setting quiet hours inside the window did not apply them: %v", limitsOf(rtorrentFake))
	}
	lastSentText(t, telegramFake, app, "limit down 3M")
	app.checkQuiet(context.Background(), at(7, 30))
	if limitsOf(rtorrentFake) != [2]uint64{3 << 20, 2 << 20} {
		t.Fatalf("after quiet hours: %v", limitsOf(rtorrentFake))
	}
}

func TestChangingOrEndingQuietHoursWhileOn(t *testing.T) {
	clock := at(1, 0)
	app, telegramFake, rtorrentFake := quietApp(t, &clock)
	lastSentText(t, telegramFake, app, "quiet 23:00-07:00 down 2M")
	lastSentText(t, telegramFake, app, "quiet 23:00-07:00 down 1M up 128K")
	if limitsOf(rtorrentFake) != [2]uint64{1 << 20, 128 << 10} {
		t.Fatalf("new quiet limits were not applied now: %v", limitsOf(rtorrentFake))
	}
	if got := lastSentText(t, telegramFake, app, "quiet off"); got != "Quiet hours are off." {
		t.Fatalf("quiet off = %q", got)
	}
	if limitsOf(rtorrentFake) != [2]uint64{10 << 20, 2 << 20} {
		t.Fatalf("turning quiet hours off did not restore the limits: %v", limitsOf(rtorrentFake))
	}
	if got := lastSentText(t, telegramFake, app, "quiet"); !strings.HasPrefix(got, "No quiet hours.") {
		t.Fatalf("quiet = %q", got)
	}
}

func TestQuietRejectsBadSchedules(t *testing.T) {
	clock := at(12, 0)
	app, telegramFake, _ := quietApp(t, &clock)
	for _, text := range []string{
		"quiet 25:00-07:00 down 1M", "quiet 23:00-23:00 down 1M", "quiet 23:00-07:00", "quiet 2300 down 1M",
	} {
		if got := lastSentText(t, telegramFake, app, text); !strings.HasPrefix(got, "quiet: ") {
			t.Errorf("%s = %q", text, got)
		}
	}
	app.state.read(func(data *stateData) {
		if data.Quiet != nil {
			t.Fatalf("a bad schedule was saved: %+v", data.Quiet)
		}
	})
}
