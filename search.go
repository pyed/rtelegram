package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// search takes a query and returns torrents with match
func search(tokens []string) {
	// make sure that we got a query
	if len(tokens) == 0 {
		send("search: needs an argument", false)
		return
	}

	query := strings.Join(tokens, " ")
	// "(?i)" for case insensitivity
	regx, err := regexp.Compile("(?i)" + query)
	if err != nil {
		logger.Print(err)
		send("search: "+err.Error(), false)
		return
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("search: "+err.Error(), false)
		return
	}

	buf := new(bytes.Buffer)
	for i := range torrents {
		if regx.MatchString(torrents[i].Name) {
			buf.WriteString(fmt.Sprintf("<%d> %s\n", i, torrents[i].Name))
		}
	}
	if buf.Len() == 0 {
		send("No matches!", false)
		return
	}
	send(buf.String(), false)
}
