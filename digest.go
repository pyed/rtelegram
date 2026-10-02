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

const (
	// maxDigestErrors is how many kinds of error a digest lists.
	maxDigestErrors = 10
	// trafficSaveInterval is how often the traffic count is saved between
	// digests; a restart of rtelegram loses at most this much counting.
	trafficSaveInterval = 10 * time.Minute
)

// digestSettings is one chat's daily digest.
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

// traffic adds up what rTorrent transfers. rTorrent reports totals since it
// started, so a restart resets them; the count carries on across restarts.
type traffic struct {
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
	// SeenUp and SeenDown are rTorrent's totals when last read.
	SeenUp   uint64 `json:"seenUp"`
	SeenDown uint64 `json:"seenDown"`
	// Since is when counting began, in Unix seconds.
	Since int64 `json:"since"`
}

// grown is how much a total grew from seen to now. A total that shrank was
// reset by an rTorrent restart, so all of it is new.
func grown(seen, now uint64) uint64 {
	if now >= seen {
		return now - seen
	}
	return now
}

// countTraffic adds what rTorrent transferred since the last look to the
// traffic count, and returns the count. It is saved when save is set or has
// not been for trafficSaveInterval; in between, it is kept in memory.
func (a *application) countTraffic(ctx context.Context, now time.Time, save bool) (traffic, error) {
	stats, err := a.rtorrent.StatsContext(ctx)
	if err != nil {
		return traffic{}, err
	}
	a.trafficMu.Lock()
	defer a.trafficMu.Unlock()
	var counted traffic
	a.state.change(func(data *stateData) {
		if data.Traffic == nil {
			data.Traffic = &traffic{SeenUp: stats.TotalUp, SeenDown: stats.TotalDown, Since: now.Unix()}
		}
		count := data.Traffic
		count.Up += grown(count.SeenUp, stats.TotalUp)
		count.Down += grown(count.SeenDown, stats.TotalDown)
		count.SeenUp, count.SeenDown = stats.TotalUp, stats.TotalDown
		counted = *count
	})
	if save || now.Sub(a.trafficSaved) >= trafficSaveInterval {
		if err := a.state.update(func(*stateData) {}); err != nil {
			a.logger.Printf("[ERROR] save the traffic count: %s", err)
		} else {
			a.trafficSaved = now
		}
	}
	return counted, nil
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
		if _, err := a.send(chatCtx, chatID, text); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "forbidden") {
				a.logger.Printf("[INFO] stopped the digest for chat %d: %s", chatID, err)
				a.state.update(func(data *stateData) { delete(data.Digest, chatID) })
			}
			continue
		}
		a.state.update(func(data *stateData) {
			if current, ok := data.Digest[chatID]; ok {
				current.LastSent, current.Since = due, now.Unix()
				current.Uploaded, current.Downloaded = counted.Up, counted.Down
				data.Digest[chatID] = current
			}
		})
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
	fmt.Fprintf(&text, "\nCompleted: %d\nAdded: %d", len(completed), len(added))

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

	if active := spaceTorrents(torrents); len(active) > 0 {
		newest := slices.MaxFunc(active, func(x, y *rtapi.Torrent) int { return cmp.Compare(x.Age, y.Age) })
		if free, err := a.rtorrent.FreeDiskSpaceContext(ctx, newest.Hash); err == nil {
			fmt.Fprintf(&text, "\n\nFree space: %s", formatBytes(free))
		}
	}
	return text.String(), counted, nil
}

// digestErrors lists the errors of torrents by tracker and message, with
// torrents that share both counted together, largest group first.
func (a *application) digestErrors(ctx context.Context, errored rtapi.Torrents) string {
	if len(errored) == 0 {
		return ""
	}
	if err := a.rtorrent.TrackersContext(ctx, errored); err != nil {
		a.logger.Printf("[ERROR] digest trackers: %s", err)
	}
	type kind struct{ tracker, message string }
	groups := make(map[kind]rtapi.Torrents)
	for _, torrent := range errored {
		key := kind{trackerHost(torrent.Tracker), strings.TrimSpace(torrent.Message)}
		groups[key] = append(groups[key], torrent)
	}
	kinds := slices.SortedFunc(maps.Keys(groups), func(x, y kind) int {
		return cmp.Or(cmp.Compare(len(groups[y]), len(groups[x])), cmp.Compare(x.tracker, y.tracker), cmp.Compare(x.message, y.message))
	})
	var text strings.Builder
	text.WriteString("\nErrors:")
	for _, key := range kinds[:min(len(kinds), maxDigestErrors)] {
		if torrents := groups[key]; len(torrents) == 1 {
			fmt.Fprintf(&text, "\n• %s: %s (%s)", key.tracker, key.message, torrents[0].Name)
		} else {
			fmt.Fprintf(&text, "\n• %s, %d torrents: %s", key.tracker, len(torrents), key.message)
		}
	}
	if more := len(kinds) - maxDigestErrors; more > 0 {
		fmt.Fprintf(&text, "\n• and %d other kinds of error", more)
	}
	return text.String()
}
