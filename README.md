# rTelegram

Manage rTorrent through a Telegram bot.

<img src="https://raw.githubusercontent.com/pyed/rtelegram/master/demo.gif" width="400" alt="rTelegram demonstration" />

## Install

Download a binary from the
[release page](https://github.com/pyed/rtelegram/releases), or build with Go
1.26 or newer:

```sh
go install github.com/pyed/rtelegram/v2@latest
```

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

3. **Find your numeric Telegram user ID.** Start the bot with your `@username`
   as a temporary master, then send it `/whoami`:

   ```sh
   RT_TOKEN=123456:secret RT_MASTERS=@yourname rtelegram -url /home/user/rtorrent/rpc.socket
   ```

   If you have no username, start it with any placeholder ID instead, such as
   `RT_MASTERS=1`, and message the bot privately. The log shows
   `Ignored a private message from unauthorized Telegram user ID ...` with your
   ID.

4. **Run it with your ID.**

   ```sh
   RT_TOKEN=123456:secret RT_MASTERS=123456789 rtelegram -url /home/user/rtorrent/rpc.socket
   ```

   Send `/help` to the bot for the command list.

## Configure

`RT_MASTERS` is a comma-separated list of the Telegram users the bot answers.
Stable numeric user IDs are preferred. Usernames are still accepted, but the bot
warns because they can be changed or reassigned. Empty or malformed entries are
rejected.

Key flags:

- `-token`, `-masters`, and `-url` override `RT_TOKEN`, `RT_MASTERS`, and
  `RT_URL`. Prefer the environment variables for secrets, since command-line
  flags are visible to other users of the machine.
- `-url` is rTorrent's address: an SCGI socket path, an SCGI `host:port`
  (default `localhost:5000`), or an `http://` or `https://` XML-RPC URL.
  Credentials in a URL are sent with HTTP basic authentication and hidden in
  logs.
- `-add-stopped` adds torrents without starting them.
- `-max-response-mib` is the largest rTorrent response the bot accepts
  (default 16). Raise it if listing a very large library fails.
- `-logfile` writes operational logs to a private file.
- `-no-live` disables follow-up message edits.
- `-version` prints the build version without requiring configuration or network
  access.
- `-completed-torrents-logfile` watches an rTorrent completion log and requires
  an explicit `-notify-chat-id` destination.
- `-data-root` enables `deldata` only beneath that absolute, same-host directory.
- `-download-root` is the rTorrent directory that upload captions may choose
  download directories under. Without it, they must be inside rTorrent's default
  directory.

## Commands

Torrents are referenced by the hash prefix that list commands show in angle
brackets, such as `<1c60cbe>`.

| Command | Alias | What it does |
|---|---|---|
| `list [tracker]` | `li` | List torrents, optionally only those whose tracker matches |
| `head [n]` / `tail [n]` | `he` / `ta` | Show the first or last n torrents (default 5) with live updates |
| `down`, `seeding`, `paused`, `checking` | `dl`, `sd`, `pa`, `ch` | List torrents in that state |
| `active` | `ac` | Show torrents currently transferring, with live updates |
| `errors` | `er` | List torrents with errors, and the error |
| `sort [rev] name\|downrate\|uprate\|size\|ratio\|age\|upload` | `so` | Set this chat's sort order |
| `trackers` | `tr` | Count torrents per tracker |
| `search QUERY` | `se` | List torrents whose name contains QUERY |
| `latest [n]` | `la` | List the n most recently added torrents |
| `add URL...` | `ad` | Add torrents from URLs or magnet links, and confirm rTorrent loaded them |
| `info HASH...` | `in` | Show details, with live updates |
| `start`, `stop`, `check` `HASH...\|all` | `st`, `sp`, `ck` | Start, stop, or verify torrents |
| `del HASH...` | | Remove torrents from rTorrent and keep their data |
| `deldata HASH confirm` | | Remove a torrent and its data (see below) |
| `stats`, `speed`, `count` | `sa`, `ss`, `co` | Show totals, current speeds, or torrents per state |
| `whoami` | | Show your user ID and this chat's ID |
| `help`, `version` | | |

To add a `.torrent` file, send it to the bot. In a private chat the caption can
set the download directory and label, as `d=/path` and `l=label`. A single other
word sets the label, or the directory if it contains a slash; longer notes are
ignored. The directory must be inside the download root (see `-download-root`),
and a relative one is placed under it. In a group, the file needs `/add` as its
caption. Files are limited to 16 MiB. The bot
downloads the file inside the Telegram trust boundary and passes raw bytes to
rTorrent, so the bot token is never embedded in an SCGI request.

The bot replies `Added:` only once the torrent appears in rTorrent, and says so
when it is already loaded. rTorrent fetches links in the background; if nothing
appears within 15 seconds, the bot reports that instead.

Group commands must start with `/`, and replies to commands in forum topics stay
in the same topic.

rTorrent multicalls are not transactional. If a batched start, stop, check, or
metadata deletion fails, the bot warns that some selected torrents may already
have changed and tells the operator to refresh before retrying.

Replies longer than three messages arrive as a text file. When Telegram limits
how fast the bot may send, the bot waits as long as Telegram asks and retries.

## Completion notifications

Have rTorrent log each finished torrent's name to a file by adding this to
`rtorrent.rc`:

```
method.set_key = event.download.finished, log_completed, \
	"execute.nothrow = sh, -c, \"echo >> /path/to/completed.log \\\"$0\\\"\", $d.name="
```

Then start rtelegram with `-completed-torrents-logfile=/path/to/completed.log`
and `-notify-chat-id` set to the chat that should receive notifications. Send
`/whoami` in that chat to see its ID.

## Deleting data

`deldata HASH confirm` is intentionally stricter than ordinary deletion. It is
disabled without `-data-root`, rejects roots, parents, symlink targets, and paths
that overlap another loaded torrent, and refuses whenever rTorrent has not
reported where another torrent keeps its data. rTorrent reports `d.base_path`
only for torrents it has opened, so unopened torrents are located through
`d.directory`. It then requires an acknowledged metadata deletion and removes
the contained local path. If local removal fails after metadata erasure, the bot
reports that partial outcome explicitly.

## Upgrading from v1

- Install from `github.com/pyed/rtelegram/v2`.
- Torrents are referenced by hash prefix, not by their position in a list.
- Prefer numeric user IDs in `RT_MASTERS`. `/whoami` shows yours.
- `-completed-torrents-logfile` now requires `-notify-chat-id`. v1 sent
  notifications to whichever chat used the bot last.
- `deldata` requires `-data-root` and the form `deldata HASH confirm`.
- In groups, commands must start with `/`, and torrent files need an `/add`
  caption.

## Security

rTorrent's RPC interface has no authentication and should never be exposed to an
untrusted network. Use a permission-protected local socket where possible and
follow rTorrent's
[official XML-RPC security guidance](https://github.com/rakshasa/rtorrent-doc/blob/master/RPC-Setup-XMLRPC.md).
Treat the Telegram token, authorized user list, notification chat ID, and
`-data-root` as security-sensitive configuration.

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
