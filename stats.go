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

Total Uploaded: *%s*
Total Download: *%s*
		`,
		throttleUp, throttleDown, stats.Port,
		humanize.IBytes(stats.TotalUp), humanize.IBytes(stats.TotalDown),
	)

	send(msg, true)
}
