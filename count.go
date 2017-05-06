package main

import (
	"fmt"

	"github.com/pyed/rtapi"
)

// count returns current torrents count per status
func count() {
	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print("count:", err)
		send("count: "+err.Error(), false)
		return
	}

	var Leeching, Seeding, Complete, Stopped, Hashing, Error int

	for i := range torrents {
		switch torrents[i].State {
		case rtapi.Leeching:
			Leeching++
		case rtapi.Seeding:
			Seeding++
		case rtapi.Complete:
			Complete++
		case rtapi.Stopped:
			Stopped++
		case rtapi.Hashing:
			Hashing++
		case rtapi.Error:
			Error++
		}
	}

	msg := fmt.Sprintf("Leeching: *%d*\nSeeding: *%d*\nComplete: *%d*\nStopped: *%d*\nHashing: *%d*\nError: *%d*\n\nTotal: *%d*",
		Leeching, Seeding, Complete, Stopped, Hashing, Error, len(torrents))

	send(msg, true)

}
