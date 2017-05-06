package main

import (
	"bytes"
	"fmt"

	humanize "github.com/pyed/go-humanize"
	"github.com/pyed/rtapi"
)

// paused will send the names of the torrents with status 'Paused'
func paused() {
	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("paused: "+err.Error(), false)
		return
	}

	buf := new(bytes.Buffer)
	for i := range torrents {
		if torrents[i].State == rtapi.Stopped {
			buf.WriteString(fmt.Sprintf("<%d> %s\n%s (%s) DL: %s UL: %s  R: %s\n\n",
				i, torrents[i].Name, torrents[i].State,
				torrents[i].Percent, humanize.Bytes(torrents[i].Completed),
				humanize.Bytes(torrents[i].UpTotal), torrents[i].Ratio))
		}
	}

	if buf.Len() == 0 {
		send("No paused torrents", false)
		return
	}

	send(buf.String(), false)
}
