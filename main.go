package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

var version = "dev"

const helpText = `Commands:
list (li) [tracker] - list torrents
head (he) [count] - list the first torrents
tail (ta) [count] - list the last torrents
down (dl), seeding (sd), paused (pa), checking (ch), active (ac), errors (er)
sort (so) [rev] name|downrate|uprate|size|ratio|age|upload
trackers (tr), search (se) QUERY, latest (la) [count]
add (ad) URL..., info (in) HASH...
stop (sp), start (st), check (ck) HASH...|all
del HASH..., deldata HASH confirm
stats (sa), speed (ss), count (co), help, version

Torrent references are the stable hash prefixes shown by list commands.
In groups, commands must start with /.`

const (
	defaultSCGIURL      = "localhost:5000"
	defaultLiveInterval = 3 * time.Second
	defaultLiveUpdates  = 5
	maxTelegramMessage  = 4096
	maxTorrentFileSize  = 16 << 20
)

type principals struct {
	ids       map[int64]struct{}
	usernames map[string]struct{}
}

type config struct {
	token           string
	masters         principals
	scgiURL         string
	logFile         string
	completedLog    string
	notifyChatID    int64
	dataRoot        string
	noLive          bool
	showVersion     bool
	legacyUsernames []string
}

type sortPreference struct {
	key     string
	reverse bool
}

type messageThreadIDKey struct{}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type application struct {
	bot          *telegram.Bot
	rtorrent     *rtapi.Rtorrent
	httpClient   httpDoer
	logger       *log.Logger
	token        string
	botUsername  string
	masters      principals
	notifyChatID int64
	dataRoot     string
	noLive       bool
	interval     time.Duration
	duration     int

	sortMu sync.RWMutex
	sorts  map[int64]sortPreference
	wg     sync.WaitGroup
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "rtelegram:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	cfg, err := parseConfig(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if cfg.showVersion {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}

	logger := log.New(stdout, "", log.LstdFlags)
	var logFile *os.File
	if cfg.logFile != "" {
		logFile, err = os.OpenFile(cfg.logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		defer logFile.Close()
		logger.SetOutput(logFile)
	}
	for _, name := range cfg.legacyUsernames {
		logger.Printf("[WARN] legacy username master @%s is mutable; prefer a numeric Telegram user ID", name)
	}

	httpClient := &http.Client{Timeout: 70 * time.Second}
	var app *application
	b, err := telegram.New(cfg.token,
		telegram.WithSkipGetMe(),
		telegram.WithHTTPClient(60*time.Second, httpClient),
		telegram.WithAllowedUpdates(telegram.AllowedUpdates{models.AllowedUpdateMessage}),
		telegram.WithNotAsyncHandlers(),
		telegram.WithErrorsHandler(func(err error) {
			logger.Printf("[ERROR] Telegram: %s", redact(cfg.token, err.Error()))
		}),
		telegram.WithDefaultHandler(func(handlerCtx context.Context, _ *telegram.Bot, update *models.Update) {
			app.handle(handlerCtx, update)
		}),
	)
	if err != nil {
		return fmt.Errorf("telegram: %s", redact(cfg.token, err.Error()))
	}

	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	me, err := b.GetMe(startupCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("telegram authorization: %s", redact(cfg.token, err.Error()))
	}
	rtorrent, err := rtapi.NewRtorrent(cfg.scgiURL)
	if err != nil {
		return fmt.Errorf("rTorrent: %w", err)
	}

	app = &application{
		bot:          b,
		rtorrent:     rtorrent,
		httpClient:   httpClient,
		logger:       logger,
		token:        cfg.token,
		botUsername:  me.Username,
		masters:      cfg.masters,
		notifyChatID: cfg.notifyChatID,
		dataRoot:     cfg.dataRoot,
		noLive:       cfg.noLive,
		interval:     defaultLiveInterval,
		duration:     defaultLiveUpdates,
		sorts:        make(map[int64]sortPreference),
	}
	logger.Printf("[INFO] Authorized as @%s; rTorrent=%s", me.Username, cfg.scgiURL)
	if cfg.completedLog != "" {
		app.launch(ctx, func(watchCtx context.Context) { app.watchCompletedLog(watchCtx, cfg.completedLog) })
	}
	b.Start(ctx)
	app.wg.Wait()
	return nil
}

func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	var cfg config
	var mastersText string
	fs := flag.NewFlagSet("rtelegram", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.token, "token", "", "Telegram bot token (or RT_TOKEN)")
	fs.StringVar(&mastersText, "masters", "", "Comma-separated Telegram user IDs or legacy usernames (or RT_MASTERS)")
	fs.StringVar(&cfg.scgiURL, "url", defaultSCGIURL, "rTorrent SCGI URL")
	fs.StringVar(&cfg.logFile, "logfile", "", "Send logs to a file")
	fs.StringVar(&cfg.completedLog, "completed-torrents-logfile", "", "Watch an rTorrent completion log")
	fs.Int64Var(&cfg.notifyChatID, "notify-chat-id", 0, "Chat ID for completion notifications")
	fs.StringVar(&cfg.dataRoot, "data-root", "", "Absolute local root allowed for deldata")
	fs.BoolVar(&cfg.noLive, "no-live", false, "Do not edit messages with live updates")
	fs.BoolVar(&cfg.showVersion, "version", false, "Print the rtelegram version and exit")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if len(fs.Args()) != 0 {
		return config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if cfg.showVersion {
		return cfg, nil
	}
	if cfg.token == "" {
		cfg.token = getenv("RT_TOKEN")
	}
	if strings.TrimSpace(cfg.token) == "" {
		return config{}, errors.New("telegram token is missing")
	}
	if mastersText == "" {
		mastersText = getenv("RT_MASTERS")
	}
	masters, legacy, err := parsePrincipals(mastersText)
	if err != nil {
		return config{}, err
	}
	cfg.masters = masters
	cfg.legacyUsernames = legacy
	if cfg.completedLog != "" && cfg.notifyChatID == 0 {
		return config{}, errors.New("-notify-chat-id is required with -completed-torrents-logfile")
	}
	if cfg.dataRoot != "" {
		if !filepath.IsAbs(cfg.dataRoot) {
			return config{}, errors.New("-data-root must be an absolute path")
		}
		cfg.dataRoot = filepath.Clean(cfg.dataRoot)
	}
	return cfg, nil
}

func parsePrincipals(text string) (principals, []string, error) {
	p := principals{ids: make(map[int64]struct{}), usernames: make(map[string]struct{})}
	if strings.TrimSpace(text) == "" {
		return p, nil, errors.New("at least one Telegram master is required")
	}
	var legacy []string
	for _, raw := range strings.Split(text, ",") {
		value := strings.TrimSpace(raw)
		if value == "" || value == "@" {
			return principals{}, nil, errors.New("telegram masters must not contain empty entries")
		}
		if id, err := strconv.ParseInt(value, 10, 64); err == nil {
			if id <= 0 {
				return principals{}, nil, fmt.Errorf("invalid Telegram user ID %q", value)
			}
			p.ids[id] = struct{}{}
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(value, "@"))
		if name == "" || strings.ContainsAny(name, "@ ") {
			return principals{}, nil, fmt.Errorf("invalid legacy Telegram username %q", value)
		}
		p.usernames[name] = struct{}{}
		legacy = append(legacy, name)
	}
	slices.Sort(legacy)
	legacy = slices.Compact(legacy)
	return p, legacy, nil
}

func (p principals) authorized(user *models.User) bool {
	if user == nil {
		return false
	}
	if _, ok := p.ids[user.ID]; ok {
		return true
	}
	if user.Username == "" {
		return false
	}
	_, ok := p.usernames[strings.ToLower(user.Username)]
	return ok
}

func parseCommand(message *models.Message, botUsername string) (string, []string, bool) {
	if message == nil {
		return "", nil, false
	}
	fields := strings.Fields(message.Text)
	if len(fields) == 0 {
		return "", nil, false
	}
	first := fields[0]
	slashed := strings.HasPrefix(first, "/")
	if message.Chat.Type != models.ChatTypePrivate && !slashed {
		return "", nil, false
	}
	first = strings.TrimPrefix(first, "/")
	command, suffix, hasSuffix := strings.Cut(first, "@")
	if hasSuffix && (suffix == "" || !strings.EqualFold(suffix, botUsername)) {
		return "", nil, false
	}
	if command == "" {
		return "", nil, false
	}
	return strings.ToLower(command), fields[1:], true
}

func documentOptions(message *models.Message, botUsername string) (string, bool) {
	if message == nil || message.Document == nil {
		return "", false
	}
	if message.Chat.Type == models.ChatTypePrivate {
		return message.Caption, true
	}
	caption := *message
	caption.Text = message.Caption
	command, arguments, ok := parseCommand(&caption, botUsername)
	if !ok || (command != "add" && command != "ad") {
		return "", false
	}
	return strings.Join(arguments, " "), true
}

func (a *application) handle(ctx context.Context, update *models.Update) {
	if update == nil || update.Message == nil || !a.masters.authorized(update.Message.From) {
		return
	}
	message := update.Message
	if message.MessageThreadID != 0 {
		ctx = context.WithValue(ctx, messageThreadIDKey{}, message.MessageThreadID)
	}
	chatID := message.Chat.ID
	if message.Document != nil {
		options, ok := documentOptions(message, a.botUsername)
		if !ok {
			return
		}
		a.receiveTorrent(ctx, chatID, message, options)
		return
	}
	command, args, ok := parseCommand(message, a.botUsername)
	if !ok {
		return
	}

	switch command {
	case "list", "li":
		a.list(ctx, chatID, args)
	case "head", "he":
		a.head(ctx, chatID, args)
	case "tail", "ta":
		a.tail(ctx, chatID, args)
	case "down", "dl":
		a.downs(ctx, chatID)
	case "seeding", "sd":
		a.seeding(ctx, chatID)
	case "paused", "pa":
		a.paused(ctx, chatID)
	case "hashing", "ha", "checking", "ch":
		a.hashing(ctx, chatID)
	case "active", "ac":
		a.active(ctx, chatID)
	case "errors", "er":
		a.errors(ctx, chatID)
	case "sort", "so":
		a.sort(ctx, chatID, args)
	case "trackers", "tr":
		a.trackers(ctx, chatID)
	case "add", "ad":
		a.add(ctx, chatID, args, "")
	case "search", "se":
		a.search(ctx, chatID, args)
	case "latest", "la":
		a.latest(ctx, chatID, args)
	case "info", "in":
		a.info(ctx, chatID, args)
	case "stop", "sp":
		a.stop(ctx, chatID, args)
	case "start", "st":
		a.start(ctx, chatID, args)
	case "check", "ck":
		a.check(ctx, chatID, args)
	case "stats", "sa":
		a.stats(ctx, chatID)
	case "speed", "ss":
		a.speed(ctx, chatID)
	case "count", "co":
		a.count(ctx, chatID)
	case "del":
		a.del(ctx, chatID, args)
	case "deldata":
		a.deldata(ctx, chatID, args)
	case "help":
		a.send(ctx, chatID, helpText)
	case "version":
		a.getVersion(ctx, chatID)
	default:
		a.send(ctx, chatID, "no such command, try /help")
	}
}

func redact(secret, text string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "[REDACTED]")
}
