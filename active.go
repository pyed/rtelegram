package main

import (
	"bytes"
	"fmt"
	"time"

	humanize "github.com/pyed/go-humanize"
	tgbotapi "gopkg.in/telegram-bot-api.v4"
)

// active will send torrents that are actively downloading or uploading
func active() {
	torrents, err := rtorrent.Torrents()
	if err != nil {
		logger.Print(err)
		send("active: "+err.Error(), false)
		return
	}

	buf := new(bytes.Buffer)
	for i := range torrents {
		if torrents[i].DownRate > 0 ||
			torrents[i].UpRate > 0 {
			torrentName := mdReplacer.Replace(torrents[i].Name) // escape markdown
			buf.WriteString(fmt.Sprintf("`<%d>` *%s*\n%s *%s* (%s) ↓ *%s*  ↑ *%s* R: *%.2f*\n\n",
				i, torrentName, torrents[i].State, humanize.IBytes(torrents[i].Completed),
				torrents[i].Percent, humanize.IBytes(torrents[i].DownRate),
				humanize.IBytes(torrents[i].UpRate), torrents[i].Ratio))
		}
	}
	if buf.Len() == 0 {
		send("No active torrents", false)
		return
	}

	msgID := send(buf.String(), true)

	if NoLive {
		return
	}

	// keep the active list live for 'duration * interval'
	for i := 0; i < duration; i++ {
		time.Sleep(time.Second * interval)
		// reset the buffer to reuse it
		buf.Reset()

		// update torrents
		torrents, err = rtorrent.Torrents()
		if err != nil {
			continue // if there was error getting torrents, skip to the next iteration
		}

		// do the same loop again
		for i := range torrents {
			if torrents[i].DownRate > 0 ||
				torrents[i].UpRate > 0 {
				torrentName := mdReplacer.Replace(torrents[i].Name) // replace markdown chars
				buf.WriteString(fmt.Sprintf("`<%d>` *%s*\n%s *%s* (%s) ↓ *%s*  ↑ *%s* R: *%.2f*\n\n",
					i, torrentName, torrents[i].State, humanize.IBytes(torrents[i].Completed),
					torrents[i].Percent, humanize.IBytes(torrents[i].DownRate),
					humanize.IBytes(torrents[i].UpRate), torrents[i].Ratio))
			}
		}

		// no need to check if it is empty, as if the buffer is empty telegram won't change the message
		editConf := tgbotapi.NewEditMessageText(chatID, msgID, buf.String())
		editConf.ParseMode = tgbotapi.ModeMarkdown
		Bot.Send(editConf)
	}
	// sleep one more time before putting the dashes
	time.Sleep(time.Second * interval)

	// replace the speed with dashes to indicate that we are done being live
	buf.Reset()
	for i := range torrents {
		if torrents[i].DownRate > 0 ||
			torrents[i].UpRate > 0 {
			// escape markdown
			torrentName := mdReplacer.Replace(torrents[i].Name)
			buf.WriteString(fmt.Sprintf("`<%d>` *%s*\n%s *%s* (%s) ↓ *-*  ↑ *-* R: *%.2f*\n\n",
				i, torrentName, torrents[i].State, humanize.IBytes(torrents[i].Completed),
				torrents[i].Percent, torrents[i].Ratio))
		}
	}

	editConf := tgbotapi.NewEditMessageText(chatID, msgID, buf.String())
	editConf.ParseMode = tgbotapi.ModeMarkdown
	Bot.Send(editConf)

}
