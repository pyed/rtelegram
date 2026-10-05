package main

import (
	"context"
	"regexp"
	"strings"

	"github.com/pyed/rtapi"
)

// A torrent is unregistered when its tracker no longer has it: a private
// tracker deleted it, or replaced it with a better release. rTorrent then
// shows the tracker's refusal as the torrent's message. Only refusals whose
// reason says so count; a tracker that cannot be reached, or refuses for
// the account's sake, does not. Only private torrents count, since public
// torrents often list trackers that refuse every torrent they were not told
// about, and find peers elsewhere anyway.

// unregisteredReasons are what trackers answer for a torrent they no longer
// have: Gazelle and Ocelot say "Unregistered torrent", XBT "Torrent not
// registered with this tracker", UNIT3D "InfoHash not found." or "Torrent
// has been deleted.", and others "Torrent not found".
var unregisteredReasons = regexp.MustCompile(`(?i)\bunregistered\b|\bnot registered\b|\b(torrent|info[ _-]?hash)( is)? not found\b|` +
	`\bunknown (torrent|info[ _-]?hash)\b|\btorrent (does not|doesn't) exist\b|\btorrent (cannot|can't|could not|couldn't) be found\b|` +
	`\btorrent (has been |was )?(deleted|removed|trumped|nuked)\b|\btrumped\b`)

// accountReasons are refusals about the account, the client, or the tracker
// itself, which may mention registration but say nothing about the torrent.
var accountReasons = regexp.MustCompile(`(?i)pass ?key|auth ?key|\bclient\b|\baccount\b|\busers?\b|\bip\b|\bbanned\b|` +
	`\bpending\b|moderat|postpone|temporar|maintenance|rate limit|too many|try again`)

// trackerRefusal returns the reason a tracker gave for refusing an announce,
// from a torrent's message, if that is what the message is. libtorrent
// writes Failure reason "REASON" when an HTTP tracker refuses, and received
// error message: REASON (tracker message: REASON in newer versions) when a
// UDP tracker does; newer versions also write Tracker warning: REASON for
// warnings that say a torrent is not registered. rTorrent wraps each as
// Tracker: [...]. Errors reaching a tracker take none of these forms.
func trackerRefusal(message string) (string, bool) {
	inner, ok := strings.CutPrefix(strings.TrimSpace(message), "Tracker: [")
	if !ok {
		return "", false
	}
	inner, ok = strings.CutSuffix(inner, "]")
	if !ok {
		return "", false
	}
	if reason, ok := strings.CutPrefix(inner, `Failure reason "`); ok {
		return strings.CutSuffix(reason, `"`)
	}
	for _, prefix := range []string{"received error message: ", "tracker message: ", "tracker warning: "} {
		if len(inner) > len(prefix) && strings.EqualFold(inner[:len(prefix)], prefix) {
			return inner[len(prefix):], true
		}
	}
	return "", false
}

// unregistered reports whether a private torrent's tracker said it no longer
// has the torrent.
func unregistered(torrent *rtapi.Torrent) bool {
	reason, ok := trackerRefusal(torrent.Message)
	return ok && torrent.Private && unregisteredReasons.MatchString(reason) && !accountReasons.MatchString(reason)
}

// countUnregistered counts the unregistered torrents among torrents.
func countUnregistered(torrents rtapi.Torrents) int {
	count := 0
	for _, torrent := range torrents {
		if unregistered(torrent) {
			count++
		}
	}
	return count
}

// unregisteredList answers /unregistered with the torrents whose tracker no
// longer has them, and buttons that remove them all, with or without data.
func (a *application) unregisteredList(ctx context.Context, chatID int64) {
	a.showList(ctx, chatID, listSpec{kind: "unregistered"})
}
