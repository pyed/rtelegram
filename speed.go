package main

import (
	"fmt"
	"time"

	humanize "github.com/pyed/go-humanize"
	tgbotapi "gopkg.in/telegram-bot-api.v4"
)

// speed will echo back the current download and upload speeds
func speed() {
	down, up := rtorrent.Speeds()

	msg := fmt.Sprintf("↓ %s  ↑ %s", humanize.IBytes(down), humanize.IBytes(up))

	msgID := send(msg, false)

	if NoLive {
		return
	}

	for i := 0; i < duration; i++ {
		time.Sleep(time.Second * interval)
		down, up = rtorrent.Speeds()

		msg = fmt.Sprintf("↓ %s  ↑ %s", humanize.IBytes(down), humanize.IBytes(up))

		editConf := tgbotapi.NewEditMessageText(chatID, msgID, msg)
		Bot.Send(editConf)
		time.Sleep(time.Second * interval)
	}
	// sleep one more time before switching to dashes
	time.Sleep(time.Second * interval)

	// show dashes to indicate that we are done updating.
	editConf := tgbotapi.NewEditMessageText(chatID, msgID, "↓ - B  ↑ - B")
	Bot.Send(editConf)
}
