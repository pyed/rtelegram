package main

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	humanize "github.com/pyed/go-humanize"
	"gopkg.in/telegram-bot-api.v4"
)

// head will list the first 5 or n torrents
func head(tokens []string) {
	var (
		n   = 5 // default to 5
		err error
	)

	if len(tokens) > 0 {
		n, err = strconv.Atoi(tokens[0])
		if err != nil {
			send("head: argument must be a number", false)
			return
		}
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("head: "+err.Error(), false)
		return
	}

	// make sure that we stay in the boundaries
	if n <= 0 || n > len(torrents) {
		n = len(torrents)
	}

	buf := new(bytes.Buffer)
	for i, torrent := range torrents[:n] {
		torrentName := mdReplacer.Replace(torrent.Name) // escape markdown
		buf.WriteString(fmt.Sprintf("`<%d>` *%s*\n%s *%s* (%s) ↓ *%s*  ↑ *%s* R: *%.2f*\n\n",
			i, torrentName, torrent.State, humanize.Bytes(torrent.Completed),
			torrent.Percent, humanize.Bytes(torrent.DownRate),
			humanize.Bytes(torrent.UpRate), torrent.Ratio))
	}

	if buf.Len() == 0 {
		send("head: No torrents", false)
		return
	}

	msgID := send(buf.String(), true)

	if NoLive {
		return
	}

	// keep the info live
	for i := 0; i < duration; i++ {
		time.Sleep(time.Second * interval)
		buf.Reset()

		torrents, err = rtorrent.Torrents()
		if err != nil {
			logger.Print("head:", err)
			continue // try again if some error heppened
		}

		if len(torrents) < 1 {
			continue
		}

		// make sure that we stay in the boundaries
		if n <= 0 || n > len(torrents) {
			n = len(torrents)
		}

		for i, torrent := range torrents[:n] {
			torrentName := mdReplacer.Replace(torrent.Name) // escape markdown
			buf.WriteString(fmt.Sprintf("`<%d>` *%s*\n%s *%s* (%s) ↓ *%s*  ↑ *%s* R: *%.2f*\n\n",
				i, torrentName, torrent.State, humanize.Bytes(torrent.Completed),
				torrent.Percent, humanize.Bytes(torrent.DownRate),
				humanize.Bytes(torrent.UpRate), torrent.Ratio))
		}

		// no need to check if it is empty, as if the buffer is empty telegram won't change the message
		editConf := tgbotapi.NewEditMessageText(chatID, msgID, buf.String())
		editConf.ParseMode = tgbotapi.ModeMarkdown
		Bot.Send(editConf)
	}

}
