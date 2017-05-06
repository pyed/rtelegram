package main

import (
	"fmt"
	"strconv"
)

// check takes id[s] of torrent[s] or 'all' to verify them
func check(tokens []string) {
	// make sure that we got at least one argument
	if len(tokens) == 0 {
		send("check: needs an argument", false)
		return
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print("check:", err)
		send("check: "+err.Error(), false)
		return
	}

	// if the first argument is 'all' then start all torrents
	if tokens[0] == "all" {
		if err := rtorrent.Check(torrents...); err != nil {
			logger.Print("check:", err)
			send("check: error occurred while verifying some torrents", false)
			return
		}
		send("hash checking all torrents", false)
		return

	}

	for _, i := range tokens {
		id, err := strconv.Atoi(i)
		if err != nil {
			send(fmt.Sprintf("check: %s is not a number", i), false)
			continue
		}

		if id >= len(torrents) || id < 0 {
			send(fmt.Sprintf("Check: No torrent with an ID of '%d'", id), false)
			continue
		}

		if err := rtorrent.Check(torrents[id]); err != nil {
			logger.Print("Check:", err)
			send("Check: "+err.Error(), false)
			continue
		}
		send(fmt.Sprintf("Checking: %s", torrents[id].Name), false)
	}
}
