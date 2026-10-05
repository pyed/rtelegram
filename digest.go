package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	telegram "github.com/go-telegram/bot"
	"github.com/pyed/rtapi"
)

const (
	// trafficSaveInterval is how often the traffic count is saved between
	// digests; a restart of rtelegram loses at most this much counting.
	trafficSaveInterval = 10 * time.Minute
)

// digestSettings is one chat's digest: daily, weekly, or monthly.
type digestSettings struct {
	Time string `json:"time"` // "08:00", in the bot's time zone
	// Every is "weekly" or "monthly"; empty is daily.
	Every  string `json:"every,omitempty"`
	Thread int    `json:"thread,omitempty"`
	// LastSent is the date the last digest was due, 2006-01-02.
	LastSent string `json:"lastSent,omitempty"`
	// Since is when the next digest's period began, in Unix seconds: when
	// the last digest was sent, or the digest was set up. Zero for digests
	// set up before rtelegram 3.0.3, whose next digest covers a day.
	Since int64 `json:"since,omitempty"`
	// Uploaded and Downloaded are the traffic count at Since.
	Uploaded   uint64 `json:"uploaded,omitempty"`
	Downloaded uint64 `json:"downloaded,omitempty"`
}

// traffic adds up the torrent data rTorrent transfers, for digests. It goes
// by each torrent's own totals, which leave out the protocol messages
// exchanged with peers: rTorrent's global totals include those, and a
// library that only seeds receives hundreds of megabytes of them a day.
type traffic struct {
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
	// SeenUp and SeenDown are the torrents' totals at the last look, and Seen
	// tells which torrents those were, so that after rtelegram restarts it
	// counts what the same torrents transferred meanwhile.
	SeenUp   uint64 `json:"seenUp"`
	SeenDown uint64 `json:"seenDown"`
	Seen     string `json:"seen,omitempty"`
	// Since is when counting began, in Unix seconds.
	Since int64 `json:"since"`
}

// growth is how much a total grew. One that shrank, as rTorrent's totals do
// when it loses some to a crash, grew by nothing.
func growth(before, now uint64) uint64 {
	return now - min(before, now)
}

// countTraffic adds what the torrents transferred since the last look to the
// traffic count, and returns the count. It is saved when save is set or has
// not been for trafficSaveInterval; in between, it is kept in memory.
func (a *application) countTraffic(ctx context.Context, now time.Time, save bool) (traffic, error) {
	transfers, err := a.rtorrent.TransfersContext(ctx)
	if err != nil {
		return traffic{}, err
	}
	looked := make(map[string]rtapi.Transfer, len(transfers))
	var up, down uint64
	for _, transfer := range transfers {
		looked[strings.ToUpper(transfer.Hash)] = transfer
		up, down = up+transfer.Up, down+transfer.Down
	}
	seen := fingerprint(slices.Collect(maps.Keys(looked)))

	a.trafficMu.Lock()
	defer a.trafficMu.Unlock()
	var counted traffic
	a.state.change(func(data *stateData) {
		if data.Traffic == nil {
			data.Traffic = &traffic{Since: now.Unix()}
		}
		count := data.Traffic
		switch {
		case a.transfers != nil:
			// A torrent's totals count from the second look at it, so that
			// neither a new torrent nor one that rTorrent is still loading
			// after a restart brings in all it ever transferred.
			for hash, transfer := range looked {
				if before, ok := a.transfers[hash]; ok {
					count.Up += growth(before.Up, transfer.Up)
					count.Down += growth(before.Down, transfer.Down)
				}
			}
		case count.Seen == seen:
			// rtelegram restarted, and rTorrent has the same torrents as at
			// the last look, so what they transferred meanwhile counts.
			count.Up += growth(count.SeenUp, up)
			count.Down += growth(count.SeenDown, down)
		}
		count.SeenUp, count.SeenDown, count.Seen = up, down, seen
		counted = *count
	})
	a.transfers = looked
	if save || now.Sub(a.trafficSaved) >= trafficSaveInterval {
		if err := a.state.update(func(*stateData) {}); err != nil {
			a.logger.Printf("[ERROR] save the traffic count: %s", err)
		} else {
			a.trafficSaved = now
		}
	}
	return counted, nil
}

// fingerprint names a set of hashes in a few characters.
func fingerprint(hashes []string) string {
	slices.Sort(hashes)
	sum := sha256.Sum256([]byte(strings.Join(hashes, ",")))
	return hex.EncodeToString(sum[:16])
}

func minutesOf(clock string) int {
	parsed, _ := time.Parse("15:04", clock)
	return parsed.Hour()*60 + parsed.Minute()
}

// digestPeriods are how often a digest can come, as /digest takes them.
var digestPeriods = []string{"daily", "weekly", "monthly"}

// period is how often the digest comes: daily, weekly, or monthly.
func (s digestSettings) period() string {
	return cmp.Or(s.Every, "daily")
}

// when says when the digest comes, as in "on Mondays at 08:00".
func (s digestSettings) when() string {
	switch s.period() {
	case "weekly":
		return "on Mondays at " + s.Time
	case "monthly":
		return "on the 1st of each month at " + s.Time
	}
	return "every day at " + s.Time
}

// lastDue returns the latest time, at or before now, when the digest was
// due: each day at its time, on Mondays for weekly digests, or on the 1st of
// the month for monthly ones.
func (s digestSettings) lastDue(now time.Time) time.Time {
	minutes := minutesOf(s.Time)
	at := func(year int, month time.Month, day int) time.Time {
		return time.Date(year, month, day, minutes/60, minutes%60, 0, 0, now.Location())
	}
	switch s.period() {
	case "weekly":
		monday := at(now.Year(), now.Month(), now.Day()-(int(now.Weekday())+6)%7)
		if monday.After(now) {
			return monday.AddDate(0, 0, -7)
		}
		return monday
	case "monthly":
		first := at(now.Year(), now.Month(), 1)
		if first.After(now) {
			return at(now.Year(), now.Month()-1, 1)
		}
		return first
	}
	today := at(now.Year(), now.Month(), now.Day())
	if today.After(now) {
		return today.AddDate(0, 0, -1)
	}
	return today
}

// digest answers /digest: HH:MM [daily|weekly|monthly] schedules it, off
// stops it, now shows one now, and no argument shows the schedule.
func (a *application) digest(ctx context.Context, chatID int64, arguments []string) {
	const usage = "digest: use digest HH:MM [daily|weekly|monthly], digest now, or digest off"
	if len(arguments) > 2 {
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
	word := ""
	if len(arguments) > 0 {
		word = strings.ToLower(arguments[0])
	}
	switch {
	case len(arguments) == 0 && current == nil:
		a.send(ctx, chatID, "No digest in this chat. Set one with digest HH:MM [daily|weekly|monthly], such as digest 08:00.")
	case len(arguments) == 0:
		a.send(ctx, chatID, fmt.Sprintf("The %s digest comes %s. %s", current.period(), current.when(), zone))
	case word == "off" && len(arguments) == 1:
		if err := a.state.update(func(data *stateData) { delete(data.Digest, chatID) }); err != nil {
			a.send(ctx, chatID, "digest: "+err.Error())
			return
		}
		a.send(ctx, chatID, "The digest is off.")
	case word == "now" && len(arguments) == 1:
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
		settings := digestSettings{Time: arguments[0], Since: now.Unix()}
		if len(arguments) == 2 {
			settings.Every = strings.ToLower(arguments[1])
		}
		if _, err := time.Parse("15:04", settings.Time); err != nil || !slices.Contains(digestPeriods, settings.period()) {
			a.send(ctx, chatID, usage)
			return
		}
		if settings.Every == "daily" {
			settings.Every = ""
		}
		counted, err := a.countTraffic(ctx, now, false)
		if err != nil {
			a.send(ctx, chatID, "digest: "+err.Error())
			return
		}
		settings.Uploaded, settings.Downloaded = counted.Up, counted.Down
		settings.Thread, _ = ctx.Value(messageThreadIDKey{}).(int)
		// The first digest comes when it is next due.
		settings.LastSent = settings.lastDue(now).Format(time.DateOnly)
		if err := a.state.update(func(data *stateData) {
			if data.Digest == nil {
				data.Digest = make(map[int64]digestSettings)
			}
			data.Digest[chatID] = settings
		}); err != nil {
			a.send(ctx, chatID, "digest: "+err.Error())
			return
		}
		a.send(ctx, chatID, fmt.Sprintf("The %s digest will come %s. %s Send digest now to see one.", settings.period(), settings.when(), zone))
	}
}

// watchDigest sends due digests and counts traffic, every minute.
func (a *application) watchDigest(ctx context.Context) {
	for {
		a.checkDigest(ctx, a.clock())
		if !waitFor(ctx, time.Minute) {
			return
		}
	}
}

// checkDigest sends each chat's digest once each time it is due, at or after
// its time, so a digest missed while the bot was offline comes as soon as it
// is back.
func (a *application) checkDigest(ctx context.Context, now time.Time) {
	var digests map[int64]digestSettings
	a.state.read(func(data *stateData) { digests = maps.Clone(data.Digest) })
	if len(digests) == 0 {
		return // traffic is counted only for digests
	}
	if _, err := a.countTraffic(ctx, now, false); err != nil {
		a.logger.Printf("[ERROR] count traffic: %s", err)
	}
	for _, chatID := range slices.Sorted(maps.Keys(digests)) {
		settings := digests[chatID]
		due := settings.lastDue(now).Format(time.DateOnly)
		if settings.LastSent >= due {
			continue
		}
		text, counted, err := a.buildDigest(ctx, now, settings)
		if err != nil {
			a.logger.Printf("[ERROR] digest: %s", err)
			continue // tried again next minute
		}
		chatCtx := ctx
		if settings.Thread != 0 {
			chatCtx = context.WithValue(ctx, messageThreadIDKey{}, settings.Thread)
		}
		// The chat may turn out to have moved; its digest moves with it.
		sentTo, _, err := a.sendText(chatCtx, chatID, text)
		switch {
		case err == nil:
			a.state.update(func(data *stateData) {
				if current, ok := data.Digest[sentTo]; ok {
					current.LastSent, current.Since = due, now.Unix()
					current.Uploaded, current.Downloaded = counted.Up, counted.Down
					data.Digest[sentTo] = current
				}
			})
		case chatGone(err):
			a.logger.Printf("[INFO] stopped the digest for chat %d: %s", sentTo, err)
			a.state.update(func(data *stateData) { delete(data.Digest, sentTo) })
		case errors.Is(err, telegram.ErrorBadRequest):
			// Telegram would refuse it again, so it is not retried; the next
			// digest also covers this one's time.
			a.logger.Printf("[ERROR] digest for chat %d not sent, and not retried: %s", sentTo, err)
			a.state.update(func(data *stateData) {
				if current, ok := data.Digest[sentTo]; ok {
					current.LastSent = due
					data.Digest[sentTo] = current
				}
			})
		}
		// Other failures, as of the network, are retried the next minute.
	}
}

// buildDigest summarizes the time since settings.Since: what finished, was
// added, and was transferred, the state of things now, and where torrents
// have errors. It returns the traffic count for the next digest to start
// from.
func (a *application) buildDigest(ctx context.Context, now time.Time, settings digestSettings) (string, traffic, error) {
	torrents, err := a.rtorrent.ListContext(ctx, rtapi.ListOptions{})
	if err != nil {
		return "", traffic{}, err
	}
	counted, err := a.countTraffic(ctx, now, false)
	if err != nil {
		return "", traffic{}, err
	}
	since := time.Unix(settings.Since, 0)
	if settings.Since == 0 {
		since = now.Add(-24 * time.Hour)
	}
	const stamp = "Mon 2 Jan 15:04"

	var text strings.Builder
	fmt.Fprintf(&text, "📰 %s digest, %s\nSince %s\n", strings.ToUpper(settings.period()[:1])+settings.period()[1:], now.Format("Mon 2 Jan"), since.Format(stamp))
	from := uint64(max(since.Unix(), 0))
	completed := filterTorrents(torrents, func(t *rtapi.Torrent) bool { return t.Finished >= from })
	added := filterTorrents(torrents, func(t *rtapi.Torrent) bool { return addedAt(t) >= from })
	fmt.Fprintf(&text, "\nCompleted: %d%s\nAdded: %d%s", len(completed), labelBreakdown(completed), len(added), labelBreakdown(added))

	if settings.Since != 0 && counted.Up >= settings.Uploaded && counted.Down >= settings.Downloaded {
		fmt.Fprintf(&text, "\n\nUploaded: %s\nDownloaded: %s", formatBytes(counted.Up-settings.Uploaded), formatBytes(counted.Down-settings.Downloaded))
	} else {
		// No digest to count from yet, so count from when counting began.
		fmt.Fprintf(&text, "\n\nUploaded: %s\nDownloaded: %s\n(counted since %s)",
			formatBytes(counted.Up), formatBytes(counted.Down), time.Unix(counted.Since, 0).Format(stamp))
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
	fmt.Fprintf(&text, "\n\nTorrents: %s", strings.Join(parts, ", "))
	text.WriteString(a.digestErrors(ctx, filterTorrents(torrents, func(t *rtapi.Torrent) bool { return t.State == rtapi.Error })))
	if count := countUnregistered(torrents); count > 0 {
		fmt.Fprintf(&text, "\nUnregistered: %d, which /unregistered removes", count)
	}

	if free, err := a.freeSpace(ctx, torrents); err == nil {
		fmt.Fprintf(&text, "\n\nFree space: %s", formatBytes(free))
	}
	return text.String(), counted, nil
}

// digestErrors lists the errors of torrents by tracker and message.
func (a *application) digestErrors(ctx context.Context, errored rtapi.Torrents) string {
	if len(errored) == 0 {
		return ""
	}
	if err := a.rtorrent.TrackersContext(ctx, errored); err != nil {
		a.logger.Printf("[ERROR] digest trackers: %s", err)
	}
	return "\nErrors:" + errorGroups(errored)
}
