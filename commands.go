package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// botCommand is a command as Telegram's command menu and /help show it.
type botCommand struct {
	name, alias, description string
}

// botCommands are every command, in menu order. They make /help, the menu the
// bot registers, and the list in the README for BotFather's /setcommands.
var botCommands = []botCommand{
	{"list", "li", "List torrents; list TRACKER lists one tracker's"},
	{"active", "ac", "Show torrents transferring now, updating live"},
	{"down", "dl", "List downloading torrents"},
	{"seeding", "sd", "List seeding torrents"},
	{"paused", "pa", "List stopped torrents"},
	{"checking", "ch", "List torrents being verified"},
	{"errors", "er", "List torrents with errors, and why"},
	{"latest", "la", "List the newest torrents: latest [N]"},
	{"head", "he", "Show the first torrents, updating live: head [N]"},
	{"tail", "ta", "Show the last torrents, updating live: tail [N]"},
	{"search", "se", "Find torrents by name: search WORDS"},
	{"info", "in", "Show torrents' cards with buttons: info HASH..."},
	{"add", "ad", "Add torrents from links or magnets: add LINK..."},
	{"find", "", "Search the indexer and add a result: find WORDS"},
	{"watch", "", "Add new releases automatically: watch add NAME WORDS, watch del NAME"},
	{"files", "fi", "Skip, prioritize, or send a torrent's files: files HASH [WORDS]"},
	{"get", "", "Send a finished file: get HASH [N]"},
	{"start", "st", "Start torrents: start HASH... or start all"},
	{"stop", "sp", "Stop torrents: stop HASH... or stop all"},
	{"check", "ck", "Verify torrents' data: check HASH... or check all"},
	{"del", "", "Remove torrents and keep their data: del HASH..."},
	{"deldata", "", "Remove a torrent and delete its data: deldata HASH"},
	{"speed", "ss", "Show current speeds, updating live"},
	{"limit", "", "Show or set speed limits: limit down 5M up 1M, or limit off"},
	{"quiet", "", "Lower speed limits at night: quiet 01:00-07:00 down 1M up 500K, or quiet off"},
	{"stats", "sa", "Show transfer totals and rTorrent settings"},
	{"count", "co", "Count torrents in each state"},
	{"trackers", "tr", "Count torrents per tracker"},
	{"sort", "so", "Sort lists: sort [rev] name|downrate|uprate|size|ratio|age|upload"},
	{"notify", "", "Choose this chat's notifications: notify [on|off]"},
	{"digest", "", "Get a summary every day, week, or month: digest 08:00 [weekly|monthly], digest now, or digest off"},
	{"whoami", "", "Show your Telegram user ID and this chat's ID"},
	{"version", "", "Show the rtelegram and rTorrent versions"},
	{"help", "", "List the commands"},
}

// helpText is the reply to /help.
var helpText = func() string {
	var text strings.Builder
	for _, command := range botCommands {
		text.WriteString("/" + command.name)
		if command.alias != "" {
			text.WriteString(" (" + command.alias + ")")
		}
		text.WriteString(" - " + command.description + "\n")
	}
	text.WriteString("\nTap the buttons under lists, or name torrents by the hash prefixes lists show.\n" +
		"In private chats the / is optional; in groups, commands must start with it.")
	return text.String()
}()

// botFatherCommands is the command list in the form BotFather's /setcommands
// takes.
func botFatherCommands() string {
	var text strings.Builder
	for _, command := range botCommands {
		fmt.Fprintf(&text, "%s - %s\n", command.name, command.description)
	}
	return text.String()
}

// registerCommands gives the bot its command menu, so that typing / in
// Telegram offers the commands, when it has none, and keeps rtelegram's own
// menu up to date: one with the commands this version has, or those the bot
// last registered, as an earlier version did. A menu with other commands,
// set in BotFather, is kept.
func (a *application) registerCommands(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	current, err := a.bot.GetMyCommands(ctx, &telegram.GetMyCommandsParams{})
	if err != nil {
		a.logger.Printf("[WARN] Could not set the command menu: %s", redact(a.token, err.Error()))
		return
	}
	menu := make([]models.BotCommand, len(botCommands))
	for i, command := range botCommands {
		menu[i] = models.BotCommand{Command: command.name, Description: command.description}
	}
	var registered []string
	a.state.read(func(data *stateData) { registered = data.Menu })
	names := commandNames(current)
	ours := len(current) == 0 || slices.Equal(names, commandNames(menu)) || slices.Equal(names, registered)
	if !ours || slices.Equal(current, menu) {
		return
	}
	if _, err := a.bot.SetMyCommands(ctx, &telegram.SetMyCommandsParams{Commands: menu}); err != nil {
		a.logger.Printf("[WARN] Could not set the command menu: %s", redact(a.token, err.Error()))
		return
	}
	if err := a.state.update(func(data *stateData) { data.Menu = commandNames(menu) }); err != nil {
		a.logger.Printf("[ERROR] Saving the command menu: %s", err)
	}
	a.logger.Printf("[INFO] Registered the command menu with Telegram")
}

func commandNames(menu []models.BotCommand) []string {
	names := make([]string, len(menu))
	for i, command := range menu {
		names[i] = command.Command
	}
	return names
}
