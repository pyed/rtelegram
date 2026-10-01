package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/pyed/rtapi"
)

// maxDigestNames is how many torrents a digest names per list.
const maxDigestNames = 10

// digestSettings is one chat's daily digest.
type digestSettings struct {
	Time     string `json:"time"` // "08:00", in the bot's time zone
	Thread   int    `json:"thread,omitempty"`
	LastSent string `json:"lastSent,omitempty"` // the date of the last digest, 2006-01-02
	// Up and Down are rTorrent's session totals when the last digest was
	// sent, to report the difference in the next.
	Up   uint64 `json:"up,omitempty"`
	Down uint64 `json:"down,omitempty"`
}

func minutesOf(clock string) int {
	parsed, _ := time.Parse("15:04", clock)
	return parsed.Hour()*60 + parsed.Minute()
}

// digest answers /digest: HH:MM schedules it, off stops it, now sends one
// now, and no argument shows the schedule.
func (a *application) digest(ctx context.Context, chatID int64, arguments []string) {
	const usage = "digest: use digest HH:MM, digest now, or digest off"
	if len(arguments) > 1 {
		a.send(ctx, chatID, usage)
		return
	}
	now := a.clock()
	zone := "Times are in the bot's time zone, " + now.Format("MST") + "."
	var current *digestSettings
	a.state.read(func(data *stateData) {
		if settings, ok := data.Digest[chatID]; ok {
			current = &settings
		}
	})
	switch {
	case len(arguments) == 0 && current == nil:
		a.send(ctx, chatID, "No daily digest in this chat. Set one with digest HH:MM, such as digest 08:00.")
	case len(arguments) == 0:
		a.send(ctx, chatID, fmt.Sprintf("The daily digest comes at %s. %s", current.Time, zone))
	case strings.EqualFold(arguments[0], "off"):
		if err := a.state.update(func(data *stateData) { delete(data.Digest, chatID) }); err != nil {
			a.send(ctx, chatID, "digest: "+err.Error())
			return
		}
		a.send(ctx, chatID, "The daily digest is off.")
	case strings.EqualFold(arguments[0], "now"):
		settings := digestSettings{}
		if current != nil {
			settings = *current
		}
		text, _, err := a.buildDigest(ctx, now, settings)
		if err != nil {
			a.send(ctx, chatID, "digest: "+err.Error())
			return
		}
		a.send(ctx, chatID, text)
	default:
		if _, err := time.Parse("15:04", arguments[0]); err != nil {
			a.send(ctx, chatID, usage)
			return
		}
		stats, err := a.rtorrent.StatsContext(ctx)
		if err != nil {
			a.send(ctx, chatID, "digest: "+err.Error())
			return
		}
		settings := digestSettings{Time: arguments[0], Up: stats.TotalUp, Down: stats.TotalDown}
		settings.Thread, _ = ctx.Value(messageThreadIDKey{}).(int)
		// If today's time has passed, the first digest comes tomorrow.
		if now.Hour()*60+now.Minute() >= minutesOf(settings.Time) {
			settings.LastSent = now.Format(time.DateOnly)
		}
		if err := a.state.update(func(data *stateData) {
			if data.Digest == nil {
				data.Digest = make(map[int64]digestSettings)
			}
			data.Digest[chatID] = settings
		}); err != nil {
			a.send(ctx, chatID, "digest: "+err.Error())
			return
		}
		a.send(ctx, chatID, fmt.Sprintf("The daily digest will come at %s. %s Send digest now to see one.", settings.Time, zone))
	}
}

// watchDigest sends due digests, checking every minute.
func (a *application) watchDigest(ctx context.Context) {
	for {
		a.checkDigest(ctx, a.clock())
		if !waitFor(ctx, time.Minute) {
			return
		}
	}
}

// checkDigest sends each chat's digest once a day, at or after its time, so
// a digest missed while the bot was offline comes as soon as it is back.
func (a *application) checkDigest(ctx context.Context, now time.Time) {
	var digests map[int64]digestSettings
	a.state.read(func(data *stateData) { digests = maps.Clone(data.Digest) })
	today := now.Format(time.DateOnly)
	for _, chatID := range slices.Sorted(maps.Keys(digests)) {
		settings := digests[chatID]
		if settings.LastSent == today || now.Hour()*60+now.Minute() < minutesOf(settings.Time) {
			continue
		}
		text, totals, err := a.buildDigest(ctx, now, settings)
		if err != nil {
			a.logger.Printf("[ERROR] digest: %s", err)
			continue // tried again next minute
		}
		chatCtx := ctx
		if settings.Thread != 0 {
			chatCtx = context.WithValue(ctx, messageThreadIDKey{}, settings.Thread)
		}
		if _, err := a.send(chatCtx, chatID, text); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "forbidden") {
				a.logger.Printf("[INFO] stopped the digest for chat %d: %s", chatID, err)
				a.state.update(func(data *stateData) { delete(data.Digest, chatID) })
			}
			continue
		}
		a.state.update(func(data *stateData) {
			if current, ok := data.Digest[chatID]; ok {
				current.LastSent, current.Up, current.Down = today, totals[0], totals[1]
				data.Digest[chatID] = current
			}
		})
	}
}

// buildDigest summarizes the last day: what finished and was added, what was
// transferred since the previous digest, and the state of things now. It
// returns rTorrent's session totals for the next digest to compare with.
func (a *application) buildDigest(ctx context.Context, now time.Time, settings digestSettings) (string, [2]uint64, error) {
	torrents, err := a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
	if err != nil {
		return "", [2]uint64{}, err
	}
	stats, err := a.rtorrent.StatsContext(ctx)
	if err != nil {
		return "", [2]uint64{}, err
	}
	since := uint64(now.Add(-24 * time.Hour).Unix())

	var text strings.Builder
	fmt.Fprintf(&text, "📰 Daily digest, %s\n", now.Format("Mon 2 Jan"))
	completed := filterTorrents(torrents, func(t *rtapi.Torrent) bool { return t.Finished >= since })
	slices.SortStableFunc(completed, func(x, y *rtapi.Torrent) int { return cmp.Compare(x.Finished, y.Finished) })
	added := filterTorrents(torrents, func(t *rtapi.Torrent) bool { return t.Age >= since })
	slices.SortStableFunc(added, func(x, y *rtapi.Torrent) int { return cmp.Compare(x.Age, y.Age) })
	for _, group := range []struct {
		label    string
		torrents rtapi.Torrents
	}{{"Completed", completed}, {"Added", added}} {
		if len(group.torrents) == 0 {
			fmt.Fprintf(&text, "\n%s: none", group.label)
			continue
		}
		fmt.Fprintf(&text, "\n%s (%d): %s", group.label, len(group.torrents), digestNames(group.torrents))
	}

	if settings.LastSent != "" && stats.TotalUp >= settings.Up && stats.TotalDown >= settings.Down {
		fmt.Fprintf(&text, "\n\nUploaded %s and downloaded %s since the last digest.",
			formatBytes(stats.TotalUp-settings.Up), formatBytes(stats.TotalDown-settings.Down))
	} else {
		fmt.Fprintf(&text, "\n\nUploaded %s and downloaded %s since rTorrent started.",
			formatBytes(stats.TotalUp), formatBytes(stats.TotalDown))
	}

	counts := make(map[string]int)
	for _, torrent := range torrents {
		counts[torrent.State]++
	}
	var parts []string
	for _, state := range []struct{ state, label string }{
		{rtapi.Leeching, "downloading"}, {rtapi.Seeding, "seeding"}, {rtapi.Complete, "complete"},
		{rtapi.Stopped, "stopped"}, {rtapi.Hashing, "checking"}, {rtapi.Error, "with errors"},
	} {
		if counts[state.state] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[state.state], state.label))
		}
	}
	if len(parts) == 0 {
		parts = []string{"none"}
	}
	fmt.Fprintf(&text, "\nTorrents: %s.", strings.Join(parts, ", "))

	if down, up, err := a.rtorrent.SpeedsContext(ctx); err == nil {
		fmt.Fprintf(&text, "\nNow: ↓ %s/s ↑ %s/s", formatBytes(down), formatBytes(up))
	}
	if active := spaceTorrents(torrents); len(active) > 0 {
		newest := slices.MaxFunc(active, func(x, y *rtapi.Torrent) int { return cmp.Compare(x.Age, y.Age) })
		if free, err := a.rtorrent.FreeDiskSpaceContext(ctx, newest.Hash); err == nil {
			fmt.Fprintf(&text, "\nFree space: %s", formatBytes(free))
		}
	}
	return text.String(), [2]uint64{stats.TotalUp, stats.TotalDown}, nil
}

func digestNames(torrents rtapi.Torrents) string {
	var names []string
	for _, torrent := range torrents[:min(len(torrents), maxDigestNames)] {
		names = append(names, torrent.Name)
	}
	text := strings.Join(names, ", ")
	if more := len(torrents) - maxDigestNames; more > 0 {
		text += fmt.Sprintf(", and %d more", more)
	}
	return text
}
