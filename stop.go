package main

import (
	"fmt"
	"strconv"
)

// stop takes id[s] of torrent[s] or 'all' to stop them
func stop(tokens []string) {
	// make sure that we got at least one argument
	if len(tokens) == 0 {
		send("stop: needs an argument", false)
		return
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print("stop:", err)
		send("stop: "+err.Error(), false)
		return
	}

	// if the first argument is 'all' then stop all torrents
	if tokens[0] == "all" {
		if err := rtorrent.Stop(torrents...); err != nil {
			logger.Print("stop:", err)
			send("stop: error occurred while stopping some torrents", false)
			return
		}
		send("stopped all torrents", false)
		return
	}

	for _, i := range tokens {
		id, err := strconv.Atoi(i)
		if err != nil {
			send(fmt.Sprintf("stop: %s is not a number", i), false)
			continue
		}

		if id >= len(torrents) || id < 0 {
			send(fmt.Sprintf("stop: No torrent with an ID of '%d'", id), false)
			continue
		}

		if err := rtorrent.Stop(torrents[id]); err != nil {
			logger.Print("stop:", err)
			send("stop: "+err.Error(), false)
			continue
		}
		send(fmt.Sprintf("Stopped: %s", torrents[id].Name), false)
	}
}
