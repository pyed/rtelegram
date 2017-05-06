package main

import "gopkg.in/telegram-bot-api.v4"

// receiveTorrent gets an update that potentially has a .torrent file to add
func receiveTorrent(ud tgbotapi.Update) {
	if ud.Message.Document == nil {
		return // has no document
	}

	// get the file ID and make the config
	fconfig := tgbotapi.FileConfig{
		FileID: ud.Message.Document.FileID,
	}
	file, err := Bot.GetFile(fconfig)
	if err != nil {
		send("receiver: "+err.Error(), false)
		return
	}

	// add by file URL
	add([]string{file.Link(BotToken)}, ud.Message.Document.FileName)
}
