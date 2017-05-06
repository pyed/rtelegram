package main

import (
	"bytes"
	"fmt"

	"github.com/pyed/rtapi"
)

// hashing will send the names of torrents with the status 'Hashing'
func hashing() {
	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("hashing: "+err.Error(), false)
		return
	}

	buf := new(bytes.Buffer)
	for i := range torrents {
		if torrents[i].State == rtapi.Hashing {
			buf.WriteString(fmt.Sprintf("<%d> %s\n%s (%s)\n\n",
				i, torrents[i].Name, torrents[i].State,
				torrents[i].Percent))

		}
	}

	if buf.Len() == 0 {
		send("No torrents hashing", false)
		return
	}

	send(buf.String(), false)
}
