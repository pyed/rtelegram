package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pyed/rtapi"
	"github.com/pyed/tailer"
	"gopkg.in/telegram-bot-api.v4"
)

const (
	VERSION = "v1.1"

	HELP = `
	*list* or *li*
	Lists all the torrents, takes an optional argument which is a query to list only torrents that has a tracker matches the query, or some of it.

	*head* or *he*
	Lists the first n number of torrents, n defaults to 5 if no argument is provided.

	*tail* or *ta*
	Lists the last n number of torrents, n defaults to 5 if no argument is provided.

	*down* or *dl*
	Lists torrents with the status of Downloading or in the queue to download.

	*seeding* or *sd*
	Lists torrents with the status of Seeding or in the queue to seed.
	
	*paused* or *pa*
	Lists Paused torrents.

	*checking* or *ch*
	Lists torrents with the status of Verifying or in the queue to verify.
	
	*active* or *ac*
	Lists torrents that are actively uploading or downloading.

	*errors* or *er*
	Lists torrents with with errors along with the error message.

	*sort* or *so*
	Manipulate the sorting of the aforementioned commands, Call it without arguments for more. 

	*trackers* or *tr*
	Lists all the trackers along with the number of torrents.

	*add* or *ad*
	Takes one or many URLs or magnets to add them, You can send a .torrent file via Telegram to add it.

	*search* or *se*
	Takes a query and lists torrents with matching names.

	*latest* or *la*
	Lists the newest n torrents, n defaults to 5 if no argument is provided.

	*info* or *in*
	Takes one or more torrent's IDs to list more info about them.

	*stop* or *sp*
	Takes one or more torrent's IDs to stop them, or _all_ to stop all torrents.

	*start* or *st*
	Takes one or more torrent's IDs to start them, or _all_ to start all torrents.

	*check* or *ck*
	Takes one or more torrent's IDs to verify them, or _all_ to verify all torrents.

	*del*
	Takes one or more torrent's IDs to delete them.

	*deldata*
	Takes one or more torrent's IDs to delete them and their data.

	*stats* or *sa*
	Shows some stats
	
	*speed* or *ss*
	Shows the upload and download speeds.
	
	*count* or *co*
	Shows the torrents counts per status.

	*help*
	Shows this help message.

	*version*
	Shows version numbers.

	- Prefix commands with '/' if you want to talk to your bot in a group. 
	- report any issues [here](https://github.com/pyed/rtelegram)
	`
)

var (

	// flags
	BotToken   string
	Masters    []string
	SCGIURL    string
	LogFile    string
	ComLogFile string
	AddLogFile string
	NoLive     bool

	// telegram
	Bot     *tgbotapi.BotAPI
	Updates <-chan tgbotapi.Update

	// rTorrent
	rtorrent *rtapi.Rtorrent

	// chatID will be used to keep track of which chat to send to.
	chatID int64

	// logging
	logger = log.New(os.Stdout, "", log.LstdFlags)

	// interval in seconds for live updates, affects: "active", "info", "speed", "head", "tail"
	interval time.Duration = 3
	// duration controls how many intervals will happen
	duration = 5

	// asterisk may cause problems parsing markdown, replace it with `•`
	// affects only markdown users: info, active, head, tail
	mdReplacer = strings.NewReplacer("*", "•")
)

// init flags
func init() {
	var mastersStr string
	// define arguments and parse them.
	flag.StringVar(&BotToken, "token", "", "Telegram bot token, Can be passed via environment variable 'RT_TOKEN'")
	flag.StringVar(&mastersStr, "masters", "", "Comma-seperated Telegram handlers, The bot will only respond to them, Can be passed via environment variable 'RT_MASTERS'")
	flag.StringVar(&SCGIURL, "url", "localhost:5000", "rTorrent SCGI URL")
	flag.StringVar(&LogFile, "logfile", "", "Send logs to a file")
	flag.StringVar(&ComLogFile, "completed-torrents-logfile", "", "Watch completed torrents log file to notify upon new ones.")
	flag.StringVar(&AddLogFile, "added-torrents-logfile", "", "Watch added torrents log file to notify upon new ones.")
	flag.BoolVar(&NoLive, "no-live", false, "Don't edit and update info after sending")

	// set the usage message
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: rtelegram <-token=TOKEN> <-masters=@tuser[,@user2..]> [-url=localhost/unix]\n\n")
		fmt.Fprint(os.Stderr, "Example: rtelegram -token=1234abc -masters=user1,user2 -url=localhost:4374\n")
		fmt.Fprint(os.Stderr, "Example: RT_TOKEN=1234abc RT_MASTERS=user1 rtelegram\n\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	// if we don't have BotToken passed, check the environment variable "RT_TOKEN"
	if BotToken == "" {
		if envVar := os.Getenv("RT_TOKEN"); len(envVar) > 1 {
			BotToken = envVar
		} else {
			fmt.Fprintf(os.Stderr, "Error: Telegram Token is missing!\n")
			flag.Usage()
			os.Exit(1)
		}
	}

	// if we don't have masters passed, check the environment variable "RT_MASTERS"
	if mastersStr == "" {
		if envVar := os.Getenv("RT_MASTERS"); len(envVar) > 1 {
			mastersStr = envVar
		} else {
			fmt.Fprintf(os.Stderr, "Error: I have no masters!\n")
			flag.Usage()
			os.Exit(1)
		}
	}

	// process mastersStr into Masters
	// get rid of @ and spaces, then split on ','
	mastersStr = strings.Replace(mastersStr, "@", "", -1)
	mastersStr = strings.Replace(mastersStr, " ", "", -1)
	mastersStr = strings.ToLower(mastersStr)
	Masters = strings.Split(mastersStr, ",")

	// if we got a log file, log to it
	if LogFile != "" {
		logf, err := os.OpenFile(LogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			log.Fatal(err)
		}
		logger.SetOutput(logf)
	}

	// if we got a completed torrents log file, monitor it for torrents completion to notify upon them.
	if ComLogFile != "" {
		go func() {
			ft := tailer.RunFileTailer(ComLogFile, false, nil)

			for {
				select {
				case line := <-ft.Lines():
					// if we don't have a chatID continue
					if chatID == 0 {
						continue
					}

					//t := time.Now()
					//t.Format("Mon Jan _2 2006 15:04:05")
					//msg := fmt.Sprintf("%s - Completed: %s", t, line)
					msg := fmt.Sprintf("Completed: %s", line)
					send(msg, false)
				case err := <-ft.Errors():
					logger.Printf("[ERROR] tailing completed torrents log: %s", err)
					return
				}

			}
		}()
	}

	// if we got a added torrents log file, monitor it for torrents added to notify upon them.
	if AddLogFile != "" {
		go func() {
			ft := tailer.RunFileTailer(AddLogFile, false, nil)

			for {
				select {
				case line := <-ft.Lines():
					// if we don't have a chatID continue
					if chatID == 0 {
						continue
					}
					//t := time.Now()
					//t.Format("Mon Jan _2 2006 15:04:05")
					//msg := fmt.Sprintf("%s - Added: %s", t, line)
					msg := fmt.Sprintf("Added: %s", line)
					send(msg, false)
				case err := <-ft.Errors():
					logger.Printf("[ERROR] tailing added torrents log: %s", err)
					return
				}

			}
		}()
	}
	
	// log the flags
	logger.Printf("[INFO] Token=%s\n\t\tMasters=%s\n\t\tURL=%s",
		BotToken, Masters, SCGIURL)
}

// init telegram
func init() {
	// authorize using the token
	var err error
	Bot, err = tgbotapi.NewBotAPI(BotToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Telegram: %s\n", err)
		os.Exit(1)
	}
	logger.Printf("[INFO] Authorized: %s", Bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	Updates, err = Bot.GetUpdatesChan(u)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Telegram: %s\n", err)
		os.Exit(1)
	}
}

// init rTorrent
func init() {
	var err error
	rtorrent, err = rtapi.NewRtorrent(SCGIURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] rTorrent: %s\n", err)
		os.Exit(1)
	}
}

func main() {
	for update := range Updates {
		// ignore edited messages
		if update.Message == nil {
			continue
		}

		// ignore non-Masters
		if !aMaster(update.Message.From.UserName) {
			logger.Printf("[INFO] Ignored a message from: %s", update.Message.From.String())
			continue
		}

		// update chatID for complete notification
		if chatID != update.Message.Chat.ID {
			chatID = update.Message.Chat.ID
		}

		// tokenize the update
		tokens := strings.Split(update.Message.Text, " ")
		command := strings.ToLower(tokens[0])

		switch command {
		case "list", "/list", "li", "/li":
			go list(tokens[1:])

		case "head", "/head", "he", "/he":
			go head(tokens[1:])

		case "tail", "/tail", "ta", "/ta":
			go tail(tokens[1:])

		case "down", "/down", "dl", "/dl":
			go downs()

		case "seeding", "/seeding", "sd", "/sd":
			go seeding()

		case "paused", "/paused", "pa", "/pa":
			go paused()

		case "hashing", "/hashing", "ha", "/ha":
			go hashing()

		case "active", "/active", "ac", "/ac":
			go active()

		case "errors", "/errors", "er", "/er":
			go errors()

		case "sort", "/sort", "so", "/so":
			go sort(tokens[1:])

		case "trackers", "/trackers", "tr", "/tr":
			go trackers()

		case "add", "/add", "ad", "/ad":
			go add(tokens[1:], "")

		case "search", "/search", "se", "/se":
			go search(tokens[1:])

		case "latest", "/latest", "la", "/la":
			go latest(tokens[1:])

		case "info", "/info", "in", "/in":
			go info(tokens[1:])

		case "stop", "/stop", "sp", "/sp":
			go stop(tokens[1:])

		case "start", "/start", "st", "/st":
			go start(tokens[1:])

		case "check", "/check", "ck", "/ck":
			go check(tokens[1:])

		case "stats", "/stats", "sa", "/sa":
			go stats()

		case "speed", "/speed", "ss", "/ss":
			go speed()

		case "count", "/count", "co", "/co":
			go count()

		case "del", "/del":
			go del(tokens[1:])

		case "deldata", "/deldata":
			go deldata(tokens[1:])

		case "help", "/help":
			go send(HELP, true)

		case "version", "/version":
			go getVersion()

		case "":
			// might be a file received
			go receiveTorrent(update)

		default:
			// no such command, try help
			go send("no such command, try /help", false)

		}
	}
}

// send takes a chat id and a message to send, returns the message id of the send message
func send(text string, markdown bool) int {
	// set typing action
	action := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	Bot.Send(action)

	// check the rune count, telegram is limited to 4096 chars per message;
	// so if our message is > 4096, split it in chunks the send them.
	msgRuneCount := utf8.RuneCountInString(text)
LenCheck:
	stop := 4095
	if msgRuneCount > 4096 {
		for text[stop] != 10 { // '\n'
			stop--
		}
		msg := tgbotapi.NewMessage(chatID, text[:stop])
		msg.DisableWebPagePreview = true
		if markdown {
			msg.ParseMode = tgbotapi.ModeMarkdown
		}

		// send current chunk
		if _, err := Bot.Send(msg); err != nil {
			logger.Printf("[ERROR] Send: %s", err)
		}
		// move to the next chunk
		text = text[stop:]
		msgRuneCount = utf8.RuneCountInString(text)
		goto LenCheck
	}

	// if msgRuneCount < 4096, send it normally
	msg := tgbotapi.NewMessage(chatID, text)
	msg.DisableWebPagePreview = true
	if markdown {
		msg.ParseMode = tgbotapi.ModeMarkdown
	}

	resp, err := Bot.Send(msg)
	if err != nil {
		logger.Printf("[ERROR] Send: %s", err)
	}

	return resp.MessageID
}

// getVersion sends rTorrent/libtorrent version + rtelegram version
func getVersion() {
	send(fmt.Sprintf("rTorrent/libtorrent: *%s*\nrtelegram: *%s*", rtorrent.Version, VERSION), true)
}

// Check if []string contains string
func aMaster(name string) bool {
	name = strings.ToLower(name)
	for i := range Masters {
		if Masters[i] == name {
			return true
		}
	}
	return false
}
