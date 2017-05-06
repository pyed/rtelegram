package main

import (
	"bytes"
	"fmt"

	"github.com/pyed/rtapi"
)

// seeding will send the names of the torrents with the status 'Seeding'.
func seeding() {
	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("seeding: "+err.Error(), false)
		return
	}

	buf := new(bytes.Buffer)
	for i := range torrents {
		if torrents[i].State == rtapi.Seeding {
			buf.WriteString(fmt.Sprintf("<%d> %s\n", i, torrents[i].Name))
		}
	}

	if buf.Len() == 0 {
		send("No torrents seeding", false)
		return
	}

	send(buf.String(), false)

}
