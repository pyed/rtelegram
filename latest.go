package main

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/pyed/rtapi"
)

// latest takes n and returns the latest n torrents
func latest(tokens []string) {
	var (
		n   = 5 // default to 5
		err error
	)

	if len(tokens) > 0 {
		n, err = strconv.Atoi(tokens[0])
		if err != nil {
			send("latest: argument must be a number", false)
			return
		}
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("latest: "+err.Error(), false)
		return
	}

	// make sure that we stay in the boundaries
	if n <= 0 || n > len(torrents) {
		n = len(torrents)
	}

	// sort by age, and set reverse to true to get the latest first
	torrents.Sort(rtapi.ByAgeRev)

	buf := new(bytes.Buffer)
	for i := range torrents[:n] {
		buf.WriteString(fmt.Sprintf("<%d> %s\n", i, torrents[i].Name))
	}
	if buf.Len() == 0 {
		send("latest: No torrents", false)
		return
	}
	send(buf.String(), false)
}
