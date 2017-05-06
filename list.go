package main

import (
	"bytes"
	"fmt"
	"regexp"
)

// list will form and send a list of all the torrents
// takes an optional argument which is a query to match against trackers
// to list only torrents that has a tracker that matchs.
func list(tokens []string) {
	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("list: "+err.Error(), false)
		return
	}

	buf := new(bytes.Buffer)
	// if it gets a query, it will list torrents that has trackers that match the query
	if len(tokens) != 0 {
		// (?i) for case insensitivity
		regx, err := regexp.Compile("(?i)" + tokens[0])
		if err != nil {
			send("list: "+err.Error(), false)
			return
		}

		for i := range torrents {
			if regx.MatchString(torrents[i].Tracker.Hostname()) {
				buf.WriteString(fmt.Sprintf("<%d> %s\n", i, torrents[i].Name))
			}
		}
	} else { // if we did not get a query, list all torrents
		for i := range torrents {
			buf.WriteString(fmt.Sprintf("<%d> %s\n", i, torrents[i].Name))
		}
	}

	if buf.Len() == 0 {
		// if we got a tracker query show different message
		if len(tokens) != 0 {
			send(fmt.Sprintf("list: No tracker matches: *%s*", tokens[0]), true)
			return
		}
		send("list: No torrents", false)
		return
	}

	send(buf.String(), false)
}
