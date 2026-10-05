package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"
)

// quietHours lowers the global rate limits between two times of day.
type quietHours struct {
	Start string  `json:"start"` // "23:00", in the bot's time zone
	End   string  `json:"end"`
	Down  *uint64 `json:"down,omitempty"` // nil leaves the limit as it is
	Up    *uint64 `json:"up,omitempty"`
	// Saved holds the limits from before quiet hours began, while they last.
	Saved *[2]uint64 `json:"saved,omitempty"`
	// PID is rTorrent's process ID when the quiet limits were applied. A
	// restart of rTorrent, which changes it, puts back rtorrent.rc's limits.
	PID int `json:"pid,omitempty"`
}

var (
	downPresets = []uint64{1 << 20, 5 << 20, 10 << 20, 0}
	upPresets   = []uint64{256 << 10, 1 << 20, 5 << 20, 0}
)

func formatRate(bytes uint64) string {
	if bytes == 0 {
		return "unlimited"
	}
	return formatBytes(bytes) + "/s"
}

func shortRate(bytes uint64) string {
	if bytes == 0 {
		return "∞"
	}
	return strings.TrimSuffix(strings.ReplaceAll(formatBytes(bytes), ".0 ", ""), "iB")
}

// parseLimit reads a rate such as 5M, or 0, off, or unlimited for no limit.
func parseLimit(text string) (uint64, error) {
	switch strings.ToLower(text) {
	case "off", "none", "unlimited", "∞":
		return 0, nil
	}
	return parseSize(text)
}

// parseLimits reads "down 5M up 1M", with either half optional.
func parseLimits(arguments []string) (down, up *uint64, err error) {
	if len(arguments) == 0 || len(arguments)%2 != 0 {
		return nil, nil, errors.New("use down RATE and/or up RATE, such as down 5M up 1M")
	}
	for i := 0; i < len(arguments); i += 2 {
		rate, err := parseLimit(arguments[i+1])
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", arguments[i+1], err)
		}
		switch strings.ToLower(arguments[i]) {
		case "down", "d", "↓":
			down = &rate
		case "up", "u", "↑":
			up = &rate
		default:
			return nil, nil, fmt.Errorf("%s is not down or up", arguments[i])
		}
	}
	return down, up, nil
}

// limit answers /limit: on its own it shows the limits with presets, and
// with arguments it sets them.
func (a *application) limit(ctx context.Context, chatID int64, arguments []string) {
	if len(arguments) == 1 && strings.EqualFold(arguments[0], "off") {
		arguments = []string{"down", "0", "up", "0"}
	}
	if len(arguments) > 0 {
		down, up, err := parseLimits(arguments)
		if err != nil {
			a.send(ctx, chatID, "limit: "+err.Error())
			return
		}
		if err := a.setLimits(ctx, down, up); err != nil {
			a.send(ctx, chatID, "limit: "+err.Error())
			return
		}
	}
	text, keyboard, err := a.renderLimits(ctx)
	if err != nil {
		a.send(ctx, chatID, "limit: "+err.Error())
		return
	}
	a.sendScreen(ctx, chatID, text, keyboard, &screen{limits: true})
}

// setLimits changes the global limits; nil leaves one as it is. During quiet
// hours the change is also what quiet hours restore when they end.
func (a *application) setLimits(ctx context.Context, down, up *uint64) error {
	a.quietMu.Lock()
	defer a.quietMu.Unlock()
	currentDown, currentUp, err := a.rtorrent.GlobalLimitsContext(ctx)
	if err != nil {
		return err
	}
	if down != nil {
		currentDown = *down
	}
	if up != nil {
		currentUp = *up
	}
	if err := a.rtorrent.SetGlobalLimitsContext(ctx, currentDown, currentUp); err != nil {
		return err
	}
	// The limit not set keeps what quiet hours restore, rather than taking
	// the quiet limit in force now.
	return a.state.update(func(data *stateData) {
		if data.Quiet != nil && data.Quiet.Saved != nil {
			saved := *data.Quiet.Saved
			if down != nil {
				saved[0] = *down
			}
			if up != nil {
				saved[1] = *up
			}
			data.Quiet.Saved = &saved
		}
	})
}

func (a *application) renderLimits(ctx context.Context) (string, *models.InlineKeyboardMarkup, error) {
	down, up, err := a.rtorrent.GlobalLimitsContext(ctx)
	if err != nil {
		return "", nil, err
	}
	text := fmt.Sprintf("Speed limits\n↓ %s ↑ %s", formatRate(down), formatRate(up))
	if quiet := a.quietSummary(); quiet != "" {
		text += "\n\n" + quiet
	}
	var downRow, upRow []models.InlineKeyboardButton
	for _, rate := range downPresets {
		downRow = append(downRow, button("↓ "+shortRate(rate), "lm:d:"+strconv.FormatUint(rate, 10)))
	}
	for _, rate := range upPresets {
		upRow = append(upRow, button("↑ "+shortRate(rate), "lm:u:"+strconv.FormatUint(rate, 10)))
	}
	return text, &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		downRow, upRow, {button("Remove limits", "lm:off")},
	}}, nil
}

// pressLimit applies a preset from the /limit screen.
func (a *application) pressLimit(ctx context.Context, key screenKey, choice string) (string, bool) {
	var down, up *uint64
	direction, value, _ := strings.Cut(choice, ":")
	rate, err := strconv.ParseUint(value, 10, 64)
	switch {
	case choice == "off":
		zero := uint64(0)
		down, up = &zero, &zero
	case err != nil:
		return "This button no longer applies.", true
	case direction == "d":
		down = &rate
	case direction == "u":
		up = &rate
	default:
		return "This button no longer applies.", true
	}
	if err := a.setLimits(ctx, down, up); err != nil {
		return err.Error(), true
	}
	text, keyboard, err := a.renderLimits(ctx)
	if err != nil {
		return err.Error(), true
	}
	if err := a.editScreen(ctx, key, text, keyboard); err != nil {
		return err.Error(), true
	}
	return "Limits changed", false
}

func (a *application) quietSummary() string {
	var quiet *quietHours
	a.state.read(func(data *stateData) { quiet = data.Quiet })
	if quiet == nil {
		return ""
	}
	var rates []string
	if quiet.Down != nil {
		rates = append(rates, "↓ "+formatRate(*quiet.Down))
	}
	if quiet.Up != nil {
		rates = append(rates, "↑ "+formatRate(*quiet.Up))
	}
	summary := fmt.Sprintf("Quiet hours %s–%s: %s", quiet.Start, quiet.End, strings.Join(rates, " "))
	if quiet.Saved != nil {
		summary += " (on now)"
	}
	return summary
}

// parseWindow reads "23:00-07:00".
func parseWindow(text string) (start, end string, err error) {
	start, end, ok := strings.Cut(text, "-")
	if !ok {
		return "", "", errors.New("give the hours as HH:MM-HH:MM, such as 23:00-07:00")
	}
	for _, clock := range []string{start, end} {
		if _, err := time.Parse("15:04", clock); err != nil {
			return "", "", fmt.Errorf("%s is not a time such as 23:00", clock)
		}
	}
	if start == end {
		return "", "", errors.New("quiet hours must start and end at different times")
	}
	return start, end, nil
}

// inWindow reports whether now's time of day is from start up to end, where a
// window can cross midnight.
func inWindow(start, end string, now time.Time) bool {
	minutes := func(clock string) int {
		parsed, _ := time.Parse("15:04", clock)
		return parsed.Hour()*60 + parsed.Minute()
	}
	from, to, current := minutes(start), minutes(end), now.Hour()*60+now.Minute()
	if from < to {
		return from <= current && current < to
	}
	return current >= from || current < to
}

// quiet answers /quiet: it shows, sets, or turns off quiet hours.
func (a *application) quiet(ctx context.Context, chatID int64, arguments []string) {
	a.send(ctx, chatID, a.changeQuiet(ctx, arguments))
}

// changeQuiet does what /quiet asks and returns the reply. It holds quietMu,
// so that the check each minute does not change limits meanwhile.
func (a *application) changeQuiet(ctx context.Context, arguments []string) string {
	zone := "\nTimes are in the bot's time zone, " + a.clock().Format("MST") + "."
	a.quietMu.Lock()
	defer a.quietMu.Unlock()
	var saved *[2]uint64
	a.state.read(func(data *stateData) {
		if data.Quiet != nil {
			saved = data.Quiet.Saved
		}
	})
	switch {
	case len(arguments) == 0:
		if summary := a.quietSummary(); summary != "" {
			return summary + zone
		}
		return "No quiet hours. Set them with quiet 23:00-07:00 down 2M [up 512K]."
	case len(arguments) == 1 && strings.EqualFold(arguments[0], "off"):
		// Restore the normal limits before forgetting them, so a refusal
		// leaves quiet hours on, to end as usual, rather than for good.
		if saved != nil {
			if err := a.rtorrent.SetGlobalLimitsContext(ctx, saved[0], saved[1]); err != nil {
				return "quiet: quiet hours stay on, since the normal limits could not be restored: " + err.Error()
			}
		}
		if err := a.state.update(func(data *stateData) { data.Quiet = nil }); err != nil {
			return "quiet: " + err.Error()
		}
		return "Quiet hours are off."
	}
	start, end, err := parseWindow(arguments[0])
	if err != nil {
		return "quiet: " + err.Error()
	}
	down, up, err := parseLimits(arguments[1:])
	if err != nil {
		return "quiet: " + err.Error()
	}
	// If quiet hours are on now, restore the normal limits first, so the
	// new quiet limits apply straight away.
	if saved != nil {
		if err := a.rtorrent.SetGlobalLimitsContext(ctx, saved[0], saved[1]); err != nil {
			return "quiet: " + err.Error()
		}
	}
	err = a.state.update(func(data *stateData) {
		data.Quiet = &quietHours{Start: start, End: end, Down: down, Up: up}
	})
	if err != nil {
		return "quiet: " + err.Error()
	}
	a.checkQuietLocked(ctx, a.clock())
	return a.quietSummary() + zone
}

// watchQuiet applies and lifts quiet hours, checking every minute.
func (a *application) watchQuiet(ctx context.Context) {
	for {
		a.checkQuiet(ctx, a.clock())
		if !waitFor(ctx, time.Minute) {
			return
		}
	}
}

// checkQuiet starts quiet hours when now is inside the window, saving the
// limits to restore, and restores them when now has left it. The saved limits
// live in the state file, so a restart of the bot in the middle changes
// nothing. A restart of rTorrent puts back the limits in rtorrent.rc, so
// then the quiet limits are applied again.
func (a *application) checkQuiet(ctx context.Context, now time.Time) {
	a.quietMu.Lock()
	defer a.quietMu.Unlock()
	a.checkQuietLocked(ctx, now)
}

// checkQuietLocked is checkQuiet for callers that hold quietMu.
func (a *application) checkQuietLocked(ctx context.Context, now time.Time) {
	var quiet quietHours
	var configured bool
	a.state.read(func(data *stateData) {
		if data.Quiet != nil {
			quiet, configured = *data.Quiet, true
		}
	})
	if !configured {
		return
	}
	failed := func(err error) {
		if text := err.Error(); text != a.quietError {
			a.logger.Printf("[ERROR] quiet hours: %s", text)
			a.quietError = text
		}
	}
	setQuiet := func(change func(*quietHours)) {
		if err := a.state.update(func(data *stateData) {
			if data.Quiet != nil {
				change(data.Quiet)
			}
		}); err != nil {
			a.logger.Printf("[ERROR] quiet hours: saving the limits to restore: %s", err)
		}
	}
	switch inside := inWindow(quiet.Start, quiet.End, now); {
	case inside:
		stats, err := a.rtorrent.StatsContext(ctx)
		if err != nil {
			failed(err)
			return
		}
		a.quietError = ""
		if quiet.Saved != nil {
			switch quiet.PID {
			case stats.PID:
				return // the quiet limits hold
			case 0:
				// Applied by a version that did not note rTorrent's process.
				setQuiet(func(q *quietHours) { q.PID = stats.PID })
				return
			}
		}
		down, up, err := a.rtorrent.GlobalLimitsContext(ctx)
		if err != nil {
			failed(err)
			return
		}
		quietDown, quietUp := down, up
		if quiet.Down != nil {
			quietDown = *quiet.Down
		}
		if quiet.Up != nil {
			quietUp = *quiet.Up
		}
		restarted := quiet.Saved != nil
		if !restarted {
			// Remember the limits before changing them, so that they come
			// back even if the bot restarts during quiet hours.
			setQuiet(func(q *quietHours) { q.Saved, q.PID = &[2]uint64{down, up}, stats.PID })
		}
		if err := a.rtorrent.SetGlobalLimitsContext(ctx, quietDown, quietUp); err != nil {
			failed(err)
			if !restarted {
				// Forget them again, so the next check tries once more.
				setQuiet(func(q *quietHours) { q.Saved, q.PID = nil, 0 })
			}
			return
		}
		if restarted {
			a.logger.Printf("[INFO] rTorrent restarted during quiet hours, so the quiet limits are applied again")
			setQuiet(func(q *quietHours) { q.PID = stats.PID })
		}
	case quiet.Saved != nil:
		if err := a.rtorrent.SetGlobalLimitsContext(ctx, quiet.Saved[0], quiet.Saved[1]); err != nil {
			failed(err)
			return
		}
		a.quietError = ""
		setQuiet(func(q *quietHours) { q.Saved, q.PID = nil, 0 })
	}
}
