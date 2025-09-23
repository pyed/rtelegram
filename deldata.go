package main

import (
	"fmt"
	"strconv"
)

// deldata takes an id or more, and delete the corresponding torrent/s with their data
func deldata(tokens []string) {
	// make sure that we got an argument
	if len(tokens) == 0 {
		send("deldata: needs an ID", false)
		return
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print("deldata:", err)
		send("deldata: "+err.Error(), false)
		return
	}

	// loop over tokens to read each potential id
	for _, i := range tokens {
		id, err := strconv.Atoi(i)
		if err != nil {
			send(fmt.Sprintf("deldata: %s is not an ID", i), false)
			continue
		}

		if id < 0 || id >= len(torrents) {
			send(fmt.Sprintf("deldata: No torrent with an ID of '%d'", id), false)
			continue
		}

		if err := rtorrent.Delete(true, torrents[id]); err != nil {
			logger.Print("deldata:", err)
			send("deldata: "+err.Error(), false)
			continue
		}

		send(fmt.Sprintf("Deleted with data: %s", torrents[id].Name), false)
	}
}
