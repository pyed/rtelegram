package main

import (
	"fmt"
	"path/filepath"
	"time"
)

// add takes an URL to a .torrent file to add it to rtorrent
func add(tokens []string, filename string) {
	if len(tokens) == 0 {
		send("add: needs at least one URL", false)
		return
	}

	// loop over the URL/s and add them
	// WARNING: it doesn't report error if the same torrent already added.
	for _, url := range tokens {
		if err := rtorrent.Download(url); err != nil {
			logger.Print("add:", err)
			send("add: %s"+err.Error(), false)
			continue
		}

		if filename == "" {
			filename = filepath.Base(url)
		}

		t := time.Now()
		t.Format("Mon Jan _2 2006 15:04:05")
		send(fmt.Sprintf("%s - Added: %s", t, filename), false)
	}
}
