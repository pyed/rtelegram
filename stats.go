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
		throttleUp = humanize.Bytes(stats.ThrottleUp)
	}

	if stats.ThrottleDown == 0 {
		throttleDown = "off"
	} else {
		throttleDown = humanize.Bytes(stats.ThrottleDown)
	}

	msg := fmt.Sprintf(
		`
\[Throttle  *%s* / *%s*]
\[Port *%s*]

Total Uploaded: *%s*
Total Download: *%s*
		`,
		throttleUp, throttleDown, stats.Port,
		humanize.Bytes(stats.TotalUp), humanize.Bytes(stats.ThrottleDown),
	)

	send(msg, true)
}
