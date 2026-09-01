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

rTorrent must provide a local XML-RPC SCGI endpoint. A protected Unix socket is
preferred; a loopback TCP address such as `127.0.0.1:5000` also works.

## Configure

The bot requires a token and at least one authorized Telegram user:

```sh
RT_TOKEN=123456:secret RT_MASTERS=123456789 rtelegram -url /run/user/1000/rtorrent.sock
```

`RT_MASTERS` is a comma-separated list. Stable numeric Telegram user IDs are
preferred. Legacy usernames are still accepted for compatibility, but the bot
warns because usernames can be changed or reassigned. Empty or malformed entries
are rejected.

Key flags:

- `-token` and `-masters` override `RT_TOKEN` and `RT_MASTERS`.
- `-url` selects the Unix-socket path or TCP SCGI address.
- `-logfile` writes operational logs to a private file.
- `-no-live` disables follow-up message edits.
- `-version` prints the build version without requiring configuration or network
  access.
- `-completed-torrents-logfile` watches an rTorrent completion log and requires
  an explicit `-notify-chat-id` destination.
- `-data-root` enables `deldata` only beneath that absolute, same-host directory.

Run `/help` for the command list. Torrent operations use the unique hash prefixes
shown by list commands rather than unstable list positions. Group commands must
start with `/`; a torrent document posted in a group must use `/add` (or `/ad`)
as its caption. Replies to commands in Telegram forum topics stay in the same
topic. Private-chat document captions remain download-directory/label options.

`deldata HASH confirm` is intentionally stricter than ordinary deletion. It is
disabled without `-data-root`, rejects roots, parents, symlink targets, and paths
that overlap another loaded torrent, requires an acknowledged metadata deletion,
and then removes the contained local path. If local removal fails after metadata
erasure, the bot reports that partial outcome explicitly.

rTorrent multicalls are not transactional. If a batched start, stop, check, or
metadata deletion fails, the bot warns that some selected torrents may already
have changed and tells the operator to refresh before retrying.

Telegram file uploads are limited to 16 MiB. The bot downloads the file inside
the Telegram trust boundary and passes raw bytes to rTorrent, so the bot token is
never embedded in an SCGI request.

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

Each repository remains independently testable with `GOWORK=off`. The
`rtelegram` module deliberately retains the latest published `rtapi` version
until the sibling library changes have an immutable release. Before tagging a
new `rtelegram` release, publish `rtapi`, update the `rtapi` requirement in
`rtelegram/go.mod`, run `go mod tidy`, and repeat both standalone and workspace
checks. The release configuration builds with `GOWORK=off` so a package can never
silently use an unpublished sibling checkout. Until that release-order step is
complete, standalone source builds fail closed with clear unsupported errors for
raw torrent uploads and `deldata`; do not tag rtelegram before updating the pin.
