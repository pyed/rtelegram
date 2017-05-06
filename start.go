package main

import (
	"fmt"
	"strconv"
)

// start takes id[s] of torrent[s] or 'all' to start them
func start(tokens []string) {
	// make sure that we got at least one argument
	if len(tokens) == 0 {
		send("start: needs an argument", false)
		return
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print("start:", err)
		send("start: "+err.Error(), false)
		return
	}

	// if the first argument is 'all' then start all torrents
	if tokens[0] == "all" {
		if err := rtorrent.Start(torrents...); err != nil {
			logger.Print("start:", err)
			send("start: error occurred while starting some torrents", false)
			return
		}
		send("started all torrents", false)
		return

	}

	for _, i := range tokens {
		id, err := strconv.Atoi(i)
		if err != nil {
			send(fmt.Sprintf("start: %s is not a number", i), false)
			continue
		}

		if id >= len(torrents) || id < 0 {
			send(fmt.Sprintf("start: No torrent with an ID of '%d'", id), false)
			continue
		}

		if err := rtorrent.Start(torrents[id]); err != nil {
			logger.Print("start:", err)
			send("start: "+err.Error(), false)
			continue
		}
		send(fmt.Sprintf("Started: %s", torrents[id].Name), false)
	}
}
