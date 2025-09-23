package main

import (
	"fmt"
	"strconv"
)

// del takes an id or more, and delete the corresponding torrent/s
func del(tokens []string) {
	// make sure that we got an argument
	if len(tokens) == 0 {
		send("del: needs an ID", false)
		return
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print("del:", err)
		send("del: "+err.Error(), false)
		return
	}

	// loop over tokens to read each potential id
	for _, i := range tokens {
		id, err := strconv.Atoi(i)
		if err != nil {
			send(fmt.Sprintf("del: %s is not an ID", i), false)
			continue
		}

		if id < 0 || id >= len(torrents) {
			send(fmt.Sprintf("del: No torrent with an ID of '%d'", id), false)
			continue
		}

		if err := rtorrent.Delete(false, torrents[id]); err != nil {
			logger.Print("del:", err)
			send("del: "+err.Error(), false)
			continue
		}

		send(fmt.Sprintf("Deleted: %s", torrents[id].Name), false)

	}
}
