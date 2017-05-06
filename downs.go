package main

import (
	"bytes"
	"fmt"

	"github.com/pyed/rtapi"
)

// downs will send the names of torrents with status 'Leeching'.
func downs() {
	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("downs: "+err.Error(), false)
		return
	}

	buf := new(bytes.Buffer)
	for i := range torrents {
		if torrents[i].State == rtapi.Leeching {
			buf.WriteString(fmt.Sprintf("<%d> %s\n", i, torrents[i].Name))
		}
	}

	if buf.Len() == 0 {
		send("No downloads", false)
		return
	}
	send(buf.String(), false)
}
