package main

import (
	"fmt"
	"strconv"
	"time"

	humanize "github.com/pyed/go-humanize"
	"github.com/pyed/rtapi"
	"gopkg.in/telegram-bot-api.v4"
)

// info takes an id of a torrent and returns some info about it
func info(tokens []string) {
	if len(tokens) == 0 {
		send("info: needs a torrent ID number", false)
		return
	}

	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print("info:", err)
		send("info: "+err.Error(), false)
	}

	for _, i := range tokens {
		id, err := strconv.Atoi(i)
		if err != nil {
			send(fmt.Sprintf("info: %s is not a number", i), false)
			continue
		}

		if id >= len(torrents) || id < 0 {
			send(fmt.Sprintf("start: No torrent with an ID of '%d'", id), false)
			continue
		}

		// format the info
		torrentName := mdReplacer.Replace(torrents[id].Name) // escape markdown
		info := fmt.Sprintf("*%s*\n%s *%s* (*%s*) ↓ *%s*  ↑ *%s* R: *%.2f* UP: *%s*\nAdded: *%s*, ETA: *%d*\nTracker: `%s`",
			torrentName, torrents[id].State, humanize.Bytes(torrents[id].Completed), torrents[id].Percent,
			humanize.Bytes(torrents[id].DownRate), humanize.Bytes(torrents[id].UpRate), torrents[id].Ratio,
			humanize.Bytes(torrents[id].UpTotal), time.Unix(int64(torrents[id].Age), 0).Format(time.Stamp),
			torrents[id].ETA, torrents[id].Tracker.Hostname())

		// send it
		msgID := send(info, true)

		if NoLive {
			return
		}

		// this go-routine will make the info live for 'duration * interval'
		go func(hash string, msgID int) {
			var torrent *rtapi.Torrent
			for i := 0; i < duration; i++ {
				time.Sleep(time.Second * interval)
				torrent, err = rtorrent.GetTorrent(hash)
				if err != nil {
					logger.Print("info:", err)
					return // if there's an error finding the torrent, maybe got deleted, return
				}

				torrentName := mdReplacer.Replace(torrent.Name) // escape markdown
				info := fmt.Sprintf("*%s*\n%s *%s* (*%s*) ↓ *%s*  ↑ *%s* R: *%.2f* UP: *%s*\nAdded: *%s*, ETA: *%d*\nTracker: `%s`",
					torrentName, torrent.State, humanize.Bytes(torrent.Completed), torrent.Percent,
					humanize.Bytes(torrent.DownRate), humanize.Bytes(torrent.UpRate), torrent.Ratio,
					humanize.Bytes(torrent.UpTotal), time.Unix(int64(torrent.Age), 0).Format(time.Stamp),
					torrent.ETA, torrent.Tracker.Hostname())

				// update the message
				editConf := tgbotapi.NewEditMessageText(chatID, msgID, info)
				editConf.ParseMode = tgbotapi.ModeMarkdown
				Bot.Send(editConf)

			}
			// sleep one more time before the dashes
			time.Sleep(time.Second * interval)
			// at the end write dashes to indicate that we are done being live.
			torrentName := mdReplacer.Replace(torrent.Name) // escape markdown
			info := fmt.Sprintf("*%s*\n *-* (*-%%*) ↓ *-*  ↑ *-* R: *-* UP: *-*\nAdded: *%s*, ETA: *-*\nTracker: `%s`",
				torrentName, time.Unix(int64(torrent.Age), 0).Format(time.Stamp), torrent.Tracker.Hostname())

			editConf := tgbotapi.NewEditMessageText(chatID, msgID, info)
			editConf.ParseMode = tgbotapi.ModeMarkdown
			Bot.Send(editConf)
		}(torrents[id].Hash, msgID)
	}
}
