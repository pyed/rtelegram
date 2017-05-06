package main

import (
	"bytes"
	"fmt"
	"regexp"
)

var trackerRegex = regexp.MustCompile(`[https?|udp]://([^:/]*)`)

// trackers will send a list of trackers and how many torrents each one has
func trackers() {
	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("trackers: "+err.Error(), false)
		return
	}

	trackers := make(map[string]int)

	for i := range torrents {
		currentTracker := torrents[i].Tracker.Hostname()
		n, ok := trackers[currentTracker]
		if !ok {
			trackers[currentTracker] = 1
			continue
		}
		trackers[currentTracker] = n + 1
	}

	buf := new(bytes.Buffer)
	for k, v := range trackers {
		buf.WriteString(fmt.Sprintf("%d - %s\n", v, k))
	}

	if buf.Len() == 0 {
		send("No trackers!", false)
		return
	}
	send(buf.String(), false)
}
