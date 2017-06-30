# rTelegram

#### Manage your rTorrent through Telegram.

<img src="https://raw.githubusercontent.com/pyed/rtelegram/master/demo.gif" width="400" />

## Install

Just [download](https://github.com/pyed/rtelegram/releases) the appropriate binary for your OS, place `rtelegram` in your `$PATH` and you are good to go.

Or if you have `Go` installed: `go get -u github.com/pyed/rtelegram`

## Requirements

Thanks to [pyed/rtapi](https://github.com/pyed/rtapi) You don't need a complicated webserver setup, All you need is:
* `rTorrent` compiled with the flag `--with-xmlrpc-c`. Which you probably already have.
* `scgi_port = localhost:5000` in your `rtorrent.rc` file.

And you should be good to go!

## Wiki

* [Getting started](https://github.com/pyed/rtelegram/wiki)
* [Commands](https://github.com/pyed/rtelegram/wiki/Commands)
* [How to get notifications](https://github.com/pyed/rtelegram/wiki/Notifications)