package main

import (
	"fmt"

	humanize "github.com/pyed/go-humanize"
)

// stats echo back transmission stats
func stats() {
	stats, err := rtorrent.Stats()
	if err != nil {
		logger.Print("stats:", err)
		send("stats: "+err.Error(), false)
		return
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print("stats:", err)
		send("stats: "+err.Error(), false)
		return
	}

	var totalUp, totalDown uint64
	for i := range torrents {
		totalUp += torrents[i].UpTotal
		totalDown += torrents[i].Completed
	}

	var ratio float64
	if totalDown > 0 {
		ratio = float64(totalUp) / float64(totalDown)
	}

	// show 'off' instead of 0 for throttling
	var throttleUp, throttleDown string
	if stats.ThrottleUp == 0 {
		throttleUp = "off"
	} else {
		throttleUp = humanize.IBytes(stats.ThrottleUp)
	}

	if stats.ThrottleDown == 0 {
		throttleDown = "off"
	} else {
		throttleDown = humanize.IBytes(stats.ThrottleDown)
	}

	msg := fmt.Sprintf(
		`
\[Throttle  *%s* / *%s*]
\[Port *%s*]
\[*%s*]
Total Uploaded: *%s*
Total Download: *%s*

All-time Upload: *%s*
All-time Download: *%s*
Global Ratio: *%.2f*
		`,
		throttleUp, throttleDown, stats.Port, stats.Directory,
		humanize.IBytes(stats.TotalUp), humanize.IBytes(stats.TotalDown),
		humanize.IBytes(totalUp), humanize.IBytes(totalDown), ratio,
	)

	send(msg, true)
}
