# rTelegram

Manage rTorrent through a Telegram bot.

<img src="https://raw.githubusercontent.com/pyed/rtelegram/master/demo.gif" width="400" alt="rTelegram demonstration" />

## Install

Download a binary from the
[release page](https://github.com/pyed/rtelegram/releases), or build with Go
1.26 or newer:

```sh
go install github.com/pyed/rtelegram/v3@latest
```

Then [set it up](#set-up), and let it
[start with the system](#start-with-the-system).

## Set up

1. **Give rTorrent an SCGI endpoint.** rTorrent must be built with XML-RPC
   support. Add one of these to `rtorrent.rc` and restart rTorrent:

   ```
   # A local socket (preferred). Run rtelegram as the same user as rTorrent,
   # or keep the socket in a directory only those users can reach.
   network.scgi.open_local = /home/user/rtorrent/rpc.socket

   # Or loopback TCP.
   network.scgi.open_port = 127.0.0.1:5000
   ```

   If rTorrent is only reachable through a web server's XML-RPC endpoint, as
   on many seedboxes and with ruTorrent or Transdroid, use that URL instead:
   `RT_URL=https://user:password@seedbox.example/RPC2`.

2. **Create a bot.** Message [@BotFather](https://t.me/BotFather), send
   `/newbot`, and follow the prompts. It replies with the bot's token.

3. **Find your numeric Telegram user ID.** Start the bot with a placeholder
   ID, and message it privately:

   ```sh
   RT_TOKEN=123456:secret RT_MASTERS=1 rtelegram -url /home/user/rtorrent/rpc.socket
   ```

   While the only master is the placeholder `1`, the bot replies with your
   user ID, and its log shows it too
   (`Telegram user ID ... is not a master`). Stop it with Ctrl+C.

4. **Run it with your ID.**

   ```sh
   RT_TOKEN=123456:secret RT_MASTERS=123456789 rtelegram -url /home/user/rtorrent/rpc.socket
   ```

   Type `/` in the chat for the command menu, or send `/help`.

5. **Start it with the system.** Stop it, and run the same command with
   `-install` (see [below](#start-with-the-system)).

## Start with the system

Add `-install` to the command that runs the bot:

```sh
RT_TOKEN=123456:secret RT_MASTERS=123456789 rtelegram -url /home/user/rtorrent/rpc.socket -install
```

rtelegram checks that it can reach Telegram and rTorrent, saves the settings
(the flags and `RT_*` variables it was given) to a config file only you can
read, and registers a service that runs `rtelegram -config FILE` and restarts
it if it stops. It prints where the settings and logs are, and how to restart
the service after editing the settings.

| System | Run as yourself | Run with `sudo` |
|---|---|---|
| Linux (systemd) | A user service that starts at boot, or at login if your account may not linger | A system service that starts at boot, as the user who ran `sudo` |
| macOS | A launch agent that starts when you log in | A launch daemon that starts at boot, as the user who ran `sudo` |
| Windows | | From an administrator prompt: a service that starts with Windows and logs to `rtelegram.log` beside the settings |

Install it as the user rTorrent runs as, so that it can reach rTorrent's
socket. Running `-install` again replaces the service with the new settings.
`rtelegram -uninstall` (with `sudo` if it was installed with `sudo`) stops the
service and removes it; the settings and state files are kept. On other
systems, have the init system run `rtelegram -config FILE`.

## Configure

`RT_MASTERS` is a comma-separated list of the numeric user IDs of the Telegram
users the bot answers. Usernames are refused, since they can be changed and
then claimed by someone else; [Set up](#set-up) shows how to find an ID.

Flags, with the environment variables that can replace them:

- **Connection.** `-token` (`RT_TOKEN`), `-masters` (`RT_MASTERS`), and `-url`
  (`RT_URL`). Prefer the environment variables for secrets, since command-line
  flags are visible to other users of the machine. `-url` is rTorrent's
  address: an SCGI socket path, an SCGI `host:port` (default `localhost:5000`),
  or an `http://` or `https://` XML-RPC URL whose credentials are sent with
  HTTP basic authentication and hidden in logs. `-max-response-mib` is the
  largest rTorrent response the bot accepts (default 64, enough for tens of
  thousands of torrents).
- **Adding torrents.** `-add-stopped` adds torrents without starting them.
  `-download-root` is the rTorrent directory that upload captions may choose
  download directories under; without it, they must be inside rTorrent's
  default directory.
- **Files on disk.** `-data-root` is the directory, on the machine rtelegram
  runs on, beneath which `get` sends files and `deldata` deletes them. When
  rTorrent runs on the same machine (its address is a socket or a loopback
  address such as `localhost:5000`), it defaults to rTorrent's download
  directory; for a remote rTorrent, set it to where that data is mounted
  locally, if anywhere. `-data-root off` turns both off. The log says which
  applies at startup.
- **Notifications.** `-watch-interval` is how often the bot checks rTorrent
  (default 30s), `-stall-after` how long a download may go without progress
  before it counts as stalled (default 30m; 0 turns it off), and `-low-disk`
  the free space below which it warns (default 5G; 0 turns it off).
- **Find and watch.** `-indexer-url` (`RT_INDEXER_URL`) and `-indexer-key`
  (`RT_INDEXER_KEY`) point them at a Torznab endpoint, and `-feed-interval` is
  how often watch rules search (default 15m).
- **The bot itself.** `-state` is the file where the bot keeps settings such as
  sort orders, subscriptions, quiet hours, watch rules, and digests; it
  defaults to `rtelegram/state.json` in the user's config directory, so a
  service without a home directory needs it set. `-logfile` writes logs to a
  private file, `-no-live` stops follow-up edits of `head`, `tail`, `active`,
  and `speed` replies, and `-version` prints the version without needing any
  other configuration.

Settings can also come from a file given with `-config FILE`, one per line and
named like the flags, as `-install` writes them:

```
token = 123456:secret
masters = 123456789
url = /home/user/rtorrent/rpc.socket
data-root = /srv/torrents
```

Flags on the command line override the file, and the file overrides the
environment.

## Commands

Lists come with a button for each torrent, ten to a page with ◀ ▶ to move
between pages. Tapping a torrent opens its card, with buttons to start or stop
it, verify it, label it, remove it, list its files, refresh, and go back to
the list. Removing asks for confirmation first. ☰ All under a list acts on
every torrent it shows (see [Acting on whole lists](#acting-on-whole-lists)). Only the users in `RT_MASTERS` can use
the buttons, even in groups, and the bot remembers the buttons of its last 500
messages.

Commands can also name torrents by the hash prefix that lists show in angle
brackets, such as `<1c6>`, or by fewer characters, down to one, as long as no
other torrent's hash starts with them. Lists show every torrent's hash to the
same length: the fewest characters that tell all loaded torrents apart, and at
least three. A few dozen torrents usually need three, a hundred four, and a
few thousand six or seven.

| Command | Alias | What it does |
|---|---|---|
| `list [tracker]` | `li` | List torrents, optionally only those whose tracker matches |
| `head [n]` / `tail [n]` | `he` / `ta` | Show the first or last n torrents (default 5) with live updates |
| `down`, `seeding`, `paused`, `checking` | `dl`, `sd`, `pa`, `ch` | List torrents in that state |
| `active` | `ac` | Show torrents currently transferring, with live updates |
| `errors` | `er` | List torrents with errors, and the error |
| `unregistered` | | List torrents their tracker deleted, with buttons that remove them all (see [Unregistered torrents](#unregistered-torrents)) |
| `labels [NAME\|-]` | | Count torrents per label, with a button for each; or list one label's torrents, or those without a label |
| `sort [rev] name\|downrate\|uprate\|size\|ratio\|age\|upload` | `so` | Set this chat's sort order |
| `trackers` | `tr` | Count torrents per tracker |
| `search QUERY` | `se` | List torrents whose name contains QUERY |
| `latest [n]` | `la` | List the n most recently added torrents |
| `add URL...` | `ad` | Add torrents from URLs or magnet links, and confirm rTorrent loaded them |
| `info HASH...` | `in` | Show each torrent's card |
| `files HASH [WORDS]` | `fi` | List a torrent's files, or those matching WORDS, to skip, prioritize, or send |
| `get HASH [N]` | | Send a torrent's finished file, up to 50 MB (see `-data-root`) |
| `start`, `stop`, `check` `HASH...\|all` | `st`, `sp`, `ck` | Start, stop, or verify torrents |
| `del HASH...` | | Remove torrents from rTorrent and keep their data |
| `deldata HASH [confirm]` | | Remove a torrent and its data, after asking (see below) |
| `setlabel HASH [LABEL\|-]` | | Set or remove a torrent's label, or choose one with buttons |
| `stats`, `speed`, `count` | `sa`, `ss`, `co` | Show totals, current speeds, or torrents per state |
| `status [off]` | | Pin a status message, like ruTorrent's status bar, that stays up to date (see [Status message](#status-message)) |
| `notify [on\|off]` | | Choose which notifications this chat gets |
| `limit [down N] [up N]\|off` | | Show or set the global speed limits |
| `quiet HH:MM-HH:MM down N [up N]\|off` | | Lower the limits every night |
| `digest HH:MM [weekly\|monthly]\|now\|off` | | Get a summary in this chat every day, week, or month |
| `find QUERY` | | Search an indexer and add a result with a tap |
| `watch [add NAME QUERY\|del NAME]` | | Add new releases for a search automatically |
| `whoami` | | Show your user ID and this chat's ID |
| `help`, `version` | | |

### Command menu

When the bot has no command menu, rtelegram registers its commands with
Telegram as it starts, so typing `/` in a chat lists them, and it keeps that
menu up to date as new versions change the commands. A menu with other
commands, set in BotFather, is kept. To set it yourself, send `/setcommands` to
[@BotFather](https://t.me/BotFather), choose the bot, and paste:

```text
list - List torrents; list TRACKER lists one tracker's
active - Show torrents transferring now, updating live
down - List downloading torrents
seeding - List seeding torrents
paused - List stopped torrents
checking - List torrents being verified
errors - List torrents with errors, and why
unregistered - List torrents their tracker deleted, to remove them
labels - List labels, or a label's torrents: labels [NAME]
latest - List the newest torrents: latest [N]
head - Show the first torrents, updating live: head [N]
tail - Show the last torrents, updating live: tail [N]
search - Find torrents by name: search WORDS
info - Show torrents' cards with buttons: info HASH...
add - Add torrents from links or magnets: add LINK...
find - Search the indexer and add a result: find WORDS
watch - Add new releases automatically: watch add NAME WORDS, watch del NAME
files - Skip, prioritize, or send a torrent's files: files HASH [WORDS]
get - Send a finished file: get HASH [N]
start - Start torrents: start HASH... or start all
stop - Stop torrents: stop HASH... or stop all
check - Verify torrents' data: check HASH... or check all
del - Remove torrents and keep their data: del HASH...
deldata - Remove a torrent and delete its data: deldata HASH
setlabel - Label a torrent: setlabel HASH LABEL, or setlabel HASH - to remove it
speed - Show current speeds, updating live
limit - Show or set speed limits: limit down 5M up 1M, or limit off
quiet - Lower speed limits at night: quiet 01:00-07:00 down 1M up 500K, or quiet off
stats - Show transfer totals and rTorrent settings
status - Pin a status message that stays up to date: status [off]
count - Count torrents in each state
trackers - Count torrents per tracker
sort - Sort lists: sort [rev] name|downrate|uprate|size|ratio|age|upload
notify - Choose this chat's notifications: notify [on|off]
digest - Get a summary every day, week, or month: digest 08:00 [weekly|monthly], digest now, or digest off
whoami - Show your Telegram user ID and this chat's ID
version - Show the rtelegram and rTorrent versions
help - List the commands
```

To add a `.torrent` file, send it to the bot. In a private chat the caption can
set the download directory and label, as `d=/path` and `l=label`. A single other
word sets the label, or the directory if it contains a slash; longer notes are
ignored. The directory must be inside the download root (see `-download-root`),
and a relative one is placed under it. In a group, the file needs `/add` as its
caption. Files are limited to 16 MiB. The bot downloads the file inside the
Telegram trust boundary and passes raw bytes to rTorrent, so the bot token is
never embedded in an SCGI request.

The bot replies `Added:` only once the torrent appears in rTorrent, and says so
when it is already loaded. rTorrent fetches links in the background; if nothing
appears within 15 seconds, the bot reports that instead.

Group commands must start with `/`, and replies to commands in forum topics stay
in the same topic. `/start` without torrents named, as Telegram sends it when
someone opens a chat with the bot, shows the list of commands.

rTorrent multicalls are not transactional. If a batched start, stop, check, or
metadata deletion fails, the bot warns that some selected torrents may already
have changed and tells the operator to refresh before retrying.

Replies longer than three messages arrive as a text file. When Telegram limits
how fast the bot may send, the bot waits as long as Telegram asks and retries.

## Files

`/files HASH`, or 📂 Files on a torrent's card, lists the torrent's files, ten
to a page, with their size and progress, and a numbered button for each: ✅
downloads, ⭐ downloads first, ⬜ is skipped. Tap a number to open that file,
choose ⬜ Skip, ✅ Download, or ⭐ First, and send it if it can be sent.
⬜ Skip all and ✅ Download all change every file at once, and ⏮ ⏭ jump to the
first and last pages.

`/files HASH WORDS` lists only the files whose paths contain every word, and
its buttons become ⬜ Skip these and ✅ Download these. To take a few files from
a large pack, tap ⬜ Skip all, then `/files HASH golf` and ✅ Download these.

When rtelegram can reach rTorrent's data (see `-data-root`), finished files up
to 50 MB (Telegram's limit for bots) that are inside `-data-root` are marked
📥, and their cards have a 📥 Send button; other cards say why a file cannot
be sent. `/get HASH N` sends file N directly; a single-file torrent
needs no N. Files are read only from inside `-data-root`, and symbolic links
cannot lead outside it. They go one at a time in the background, so the bot
goes on answering, and each may take up to 15 minutes on a slow uplink.

## Labels

rtelegram shows and sets the labels ruTorrent shows. ruTorrent keeps a
torrent's label in rTorrent's `d.custom1`, percent-encoded, and rtelegram reads
and writes labels the same way, so both always agree. A torrent's card shows
its label, and 🏷 Label offers the labels in use, the most used first, and
✖ No label; for a new label, reply to that message with it. `/setlabel HASH
LABEL` sets a label directly, `/setlabel HASH -` removes it, and `/setlabel
HASH` shows the choices.

`/labels` counts the torrents with each label, with a button for each that
lists them; `/labels NAME` lists one label's torrents, ignoring case, and
`/labels -` those without a label. Labels given when adding, in a `.torrent`
file's caption or a watch rule, are stored the way ruTorrent stores them.
Digests count what finished and what was added per label.

## Acting on whole lists

A list of two or more torrents ends with ☰ All, which starts, stops, verifies,
labels, or removes every torrent the list shows, with or without their data.
Each asks first, naming the torrents and how many there are. It then acts only
on the torrents the list showed when ☰ was tapped that it still shows: a
torrent that joined the list meanwhile is never touched, and the outcome says
how many left it. The list of every torrent (`/list` without a tracker) offers
no removal, so one tap can never remove the whole library; lists of a tracker,
a label, a search, or a state do.

Removing with data keeps the safeguards of `deldata` for every torrent: data
is deleted only beneath `-data-root`, never through a symbolic link, and never
while another torrent uses it. A torrent whose data overlaps that of a torrent
that stays loaded, such as the same files seeded on two trackers, is kept with
its data, unless both are being removed. So is a torrent with no data where
rTorrent says it is; remove it without its data instead. Nothing is removed
while rTorrent has not said where some torrent's data is. rtelegram removes
the torrents from rTorrent first, checks which are gone, and only then deletes
their data, one action at a time. The outcome lists what was removed, and what
was kept and why.

## Unregistered torrents

When a private tracker deletes a torrent, or replaces it with a better
release, it refuses the torrent's announces with a reason such as
`Unregistered torrent`, and rTorrent shows that as the torrent's error.
`/unregistered` lists these torrents, with 🗑 Remove all and, with
`-data-root`, 💣 Remove all + data, which ask first and work as described in
[Acting on whole lists](#acting-on-whole-lists).

Only refusals saying the tracker no longer has the torrent count: Gazelle's and
Ocelot's `Unregistered torrent`, XBT's `Torrent not registered with this
tracker`, UNIT3D's `InfoHash not found.` and `Torrent has been deleted.`, and
the like. A tracker that is down or times out never counts, nor does a refusal
about the account, the client, or the tracker itself, such as an invalid
passkey, a banned client, or maintenance. Only private torrents count: public
torrents often list trackers that refuse every torrent they were not told
about, and find peers elsewhere. The errors list and error notifications have
a 🧹 button that lists unregistered torrents, digests count them, and their
cards say so.

## Find and watch

With an indexer configured, `/find QUERY` searches it and shows the eight
results with the most seeders as buttons; tap one to add it. The indexer is any
Torznab endpoint, such as one Prowlarr indexer
(`http://prowlarr:9696/1/api`) or all of Jackett's
(`http://jackett:9117/api/v2.0/indexers/all/results/torznab/api`), with its API
key in `RT_INDEXER_KEY`. Magnet links go to rTorrent directly; the bot
downloads `.torrent` files from the indexer itself, so the key never reaches
rTorrent. Adds are confirmed as with `add`.

`/watch add NAME QUERY [l=LABEL] [d=DIR]` adds new releases for a search as
they appear, checking every `-feed-interval`. Releases already there when the
rule is made are skipped, at most five are added per check, and each is
announced in the chat that made the rule. `/watch` lists the rules and
`/watch del NAME` removes one.

## Speed limits

`/limit` shows rTorrent's global download and upload limits with buttons for
common values. `/limit down 5M up 1M` sets them (either half can be left
out), and `/limit off` removes them. Rates are per second, in binary units.

`/quiet 23:00-07:00 down 2M` lowers the limits between those times every day,
for example while others at home are streaming. Give `down`, `up`, or both;
the other limit stays as it is. When quiet hours end, the limits go back to
what they were, or to whatever `/limit` set during quiet hours. Windows can
cross midnight, times are in the bot's time zone, and quiet hours survive a
restart of the bot. A restart of rTorrent brings back the limits in its
`rtorrent.rc`, so during quiet hours the bot applies the quiet limits again.
`/quiet` shows the schedule and `/quiet off` removes it, once the normal
limits are back.

## Status message

`/status` sends a message like ruTorrent's status bar, pins it without a
notification, and keeps it up to date every minute; 🔄 Refresh updates it at
once.

```
📊 rTorrent status
↓ 2.3 MiB/s · limit 10.0 MiB/s · 12.4 GiB this session
↑ 8.1 MiB/s · no limit · 210.5 GiB this session
Torrents: 3104 · 3 downloading · 41 uploading · 152 with errors
Peers: 58 in, 22 out · port 6890 open ✅
Free space: 1.2 TiB
Updated 14:05
```

It shows the speeds and global limits, what rTorrent transferred since it
started (its own totals, which include protocol messages, as ruTorrent shows
them), how many torrents are downloading, uploading, or have errors, and peer
connections. Peers can connect in only through rTorrent's port, so one that
did shows the port is reachable (✅). With peers connected out but none in,
the port may be closed (⚠️), as when a router does not forward it. Free space
is where rTorrent saves data. During quiet hours the message says until when,
and when rTorrent stops answering, it says so.

A chat has one status message: `/status` again replaces it, and `/status off`
stops updating it and unpins it. In a group, the bot needs the right to pin
messages; without it, the message still stays up to date. The bot remembers
the message across restarts, and stops when someone deletes it.

## Notifications

Send `/notify` in any chat, private or group, to choose what the bot tells it
about. Each is a button to turn on or off:

- **Completed downloads**, with a button to open the torrent's card.
- **New errors**, such as a tracker rejecting a torrent. A message about
  several errors that include unregistered torrents has a 🧹 button that lists
  them, to remove them (see [Unregistered torrents](#unregistered-torrents)).
- **Stalled downloads**, when a download makes no progress for `-stall-after`.
- **Low disk space**, when free space where rTorrent saves data drops below
  `-low-disk`. The bot warns again only after space recovers.
- **rTorrent down or restarted**: when rTorrent has not answered for a minute,
  when it answers again and after how long, and when it restarted, which the
  bot tells from its process ID, even between two checks.

`/notify on` and `/notify off` turn everything on or off at once. In a group
with topics, notifications go to the topic `/notify` was sent from.

When more than three things of a kind happen at once, such as a batch of
downloads finishing together, they come in one message. Errors are grouped by
tracker and message: once some are announced, new ones with the same tracker
and message wait an hour, then come together unless they have cleared, and
the wait doubles, up to a day, while they keep coming. So a tracker that
fails for every torrent makes one message, not one per torrent.

The bot checks rTorrent itself every `-watch-interval`, so nothing needs to be
added to `rtorrent.rc`. Torrents that finish while the bot is offline are
announced when it starts again. Subscriptions are kept in the `-state` file. A
chat that blocks or removes the bot, or no longer exists, is unsubscribed.
When a group becomes a supergroup, which changes its ID, its notifications,
digest, and watch rules follow it, and when their forum topic is deleted, they
go to the group itself.

## Digests

`/digest 08:00` sends this chat a summary every day at that time,
`/digest 08:00 weekly` every Monday, and `/digest 08:00 monthly` on the 1st of
each month. Each covers the time since the previous digest:

```
📰 Daily digest, Fri 2 Oct
Since Thu 1 Oct 08:00

Completed: 2 (Movies 1, TV 1)
Added: 3 (TV 2, no label 1)

Uploaded: 48.2 GiB
Downloaded: 9.7 GiB

Torrents: 2887 seeding, 166 with errors
Errors:
• tracker.example, 150 torrents: Tracker: [Failure reason "Unregistered torrent"]
• other.example, 16 torrents: Tracker: [Timeout was reached]
Unregistered: 150, which /unregistered removes

Free space: 1.2 TiB
```

Torrents with errors are grouped by tracker and message. Torrents count as
added when they first started, which rTorrent remembers across restarts, and
what finished and was added is counted per label when torrents have labels.
Uploaded and Downloaded count torrent data only, from each torrent's own
totals. rTorrent's global totals, which `/stats` shows, also count the
protocol messages exchanged with peers: a large library that only seeds
receives hundreds of megabytes of those a day. Traffic while the bot is not
running counts too, as long as rTorrent keeps the same torrents meanwhile.

`/digest now` shows the digest so far without resetting it, `/digest off`
stops it, and `/digest` shows when it comes. A digest missed while the bot was
offline comes as soon as it is back, once, and in a group it goes to the topic
`/digest` was sent from. A digest Telegram refuses, as when the bot lacks the
right to post in a group, is not retried; the next one covers its time too.

## Deleting data

`deldata HASH` asks for confirmation with buttons, and `deldata HASH confirm`
deletes straight away. Either is intentionally stricter than ordinary deletion.
It works only beneath `-data-root`, rejects roots, parents, symlink targets,
and paths that overlap another loaded torrent, and refuses whenever rTorrent
has not reported where another torrent keeps its data. rTorrent reports `d.base_path`
only for torrents it has opened, so unopened torrents are located through
`d.directory`. It then requires an acknowledged metadata deletion and removes
the contained local path. If local removal fails after metadata erasure, the bot
reports that partial outcome explicitly. To remove many torrents with their
data at once, see [Acting on whole lists](#acting-on-whole-lists).

## Upgrading to v3

- Install from `github.com/pyed/rtelegram/v3`.
- `-completed-torrents-logfile` and `-notify-chat-id` are gone. Send
  `/notify` in the chat that should hear about completed downloads; the
  `rtorrent.rc` line that logged completions is no longer needed.
- From 3.0.1, `RT_MASTERS` takes only numeric user IDs; usernames are refused.
  [Set up](#set-up) shows how to find yours.
- Settings are kept in the `-state` file. Make sure its directory is writable,
  or set `-state` (for example, for a service with no home directory).
- `info` sends a card with buttons instead of editing itself for a while, and
  lists of more than ten torrents come in pages.
- `deldata HASH` now asks for confirmation; `deldata HASH confirm` still
  deletes straight away.

From v1, also:

- Torrents are referenced by hash prefix, not by their position in a list.
- `RT_MASTERS` takes numeric user IDs, not usernames.
- `deldata` works only beneath `-data-root`.
- In groups, commands must start with `/`, and torrent files need an `/add`
  caption.

## Security

rTorrent's RPC interface has no authentication and should never be exposed to an
untrusted network. Use a permission-protected local socket where possible and
follow rTorrent's
[official XML-RPC security guidance](https://github.com/rakshasa/rtorrent-doc/blob/master/RPC-Setup-XMLRPC.md).
Treat the Telegram token, the authorized user list, the indexer key, the
`-config` and `-state` files (which the bot writes with private permissions),
and `-data-root` as security-sensitive configuration. Prefer environment
variables or a config file to flags for secrets, since other users of the
machine can see a program's flags. Anyone in `RT_MASTERS` can
add torrents and, with `-data-root`, delete data and read files beneath it.

## Development and release order

The parent workspace contains both repositories and binds them with `go.work`:

```sh
go test ./rtapi/... ./rtelegram/...
```

Each repository remains independently testable with `GOWORK=off`. When
rtelegram uses an `rtapi` change that has not been released yet, the workspace
build passes but the standalone build fails until that change is published.
Before tagging a new `rtelegram` release, tag `rtapi`, update the `rtapi`
requirement in `rtelegram/go.mod`, run `go mod tidy`, and repeat both standalone
and workspace checks. The release configuration builds with `GOWORK=off` so a
release can never silently use an unpublished sibling checkout.
