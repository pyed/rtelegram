package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/pyed/rtapi"
	tgbotapi "gopkg.in/telegram-bot-api.v4"
)

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

	// if there's no options, just add the torrent
	if ud.Message.Caption == "" {
		add([]string{file.Link(BotToken)}, ud.Message.Document.FileName)
		return
	}

	var tFile rtapi.DotTorrentWithOptions
	tFile.Link = file.Link(BotToken)
	tFile.Name = ud.Message.Document.FileName
	tFile.Dir, tFile.Label = processOptions(ud.Message.Caption)

	// check if dir is there, or try to make it.
	if tFile.Dir != "" {
		// if there's '~' expand it
		if strings.HasPrefix(tFile.Dir, "~") {
			homedir, err := os.UserHomeDir()
			if err != nil {
				send(fmt.Sprintf("receiver: Couldn't expand '~' in: %s", tFile.Dir), false)
				return
			}
			tFile.Dir = strings.Replace(tFile.Dir, "~", homedir, 1)
		}

		// if the directory isn't there, create it
		if _, err := os.Stat(tFile.Dir); os.IsNotExist(err) {
			if err = os.MkdirAll(tFile.Dir, os.ModePerm); err != nil {
				send(fmt.Sprintf("receiver: Couldn't make directory %s, error: %s", tFile.Dir, err.Error()), false)
				return
			} else {
				send("New directory created: "+tFile.Dir, false)
			}
		}
	}

	// add the .torrent with options
	if err := rtorrent.DownloadWithOptions(&tFile); err != nil {
		logger.Print("add with options:", err)
		send("add with options: %s"+err.Error(), false)
	}

	send(fmt.Sprintf("Added: %s", tFile.Name), false)

}

// processOptions looks inside 'ud.Message.Caption' and processes the passed options if any;
// e.g. d=/dir/to/downlaods l=Software, will save the added torrent          ;
// torrent to the specified direcotry, and will assigne the label "Software" ;
// to it, labels are saved to "d.custom1", which is used by ruTorrent.       ;
func processOptions(options string) (dir, lable string) {
	if options == "" {
		return
	}

	// more options can be added later
	sliceOfOptions := strings.Split(options, " ")
	for _, o := range sliceOfOptions {
		switch {
		case strings.HasPrefix(o, "d="): // directory
			dir = o[2:]
		case strings.HasPrefix(o, "l="): // label
			lable = o[2:]
		case strings.ContainsAny(o, "/\\"): // maybe a directory without 'd='
			dir = o
		default: // if none of the above matches, then just make it a label
			lable = o
		}
	}
	return
}
