package main

import (
	"bytes"
	"fmt"

	"github.com/pyed/rtapi"
)

// errors will list torrents with errors
func errors() {
	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("errors: "+err.Error(), false)
		return
	}

	buf := new(bytes.Buffer)
	for i := range torrents {
		if torrents[i].State == rtapi.Error {
			buf.WriteString(fmt.Sprintf("<%d> %s\n%s\n",
				i, torrents[i].Name, torrents[i].Message))
		}
	}
	if buf.Len() == 0 {
		send("No errors", false)
		return
	}
	send(buf.String(), false)
}
