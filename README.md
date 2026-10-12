# zotty

zotty (zero-overhead-tty) is a small terminal bridge for the zot web UI ([web.zot.sh](https://web.zot.sh)) or similar. It runs on the same machine as your zot host and gives the web UI a real shell in a pseudo-terminal. zot itself is not involved or changed.

> **Security:** zotty grants full shell access as the user running it. Anyone who can reach it and has the token can run any command. Keep it on localhost and expose it only through a private tunnel such as Tailscale. Do not publish it through a public tunnel.

## Run

```sh
go build -o zotty .
./zotty serve
```

On first start it creates a random token in your user config directory (macOS: `~/Library/Application Support/zotty/token`, mode `0600`) and prints it. In the web UI, open the terminal button in the sidebar footer and enter the bridge URL and this token.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--listen` | `127.0.0.1:7789` | Address to listen on. A non-loopback address logs a warning. |
| `--token-file` | user config dir | Token file, created if missing. |
| `--origins` | `https://web.zot.sh` | Comma-separated browser origins allowed in addition to localhost. |
| `--shell` | `$SHELL`, then `/bin/sh` | Shell to start as a login shell. |
| `--dir` | home directory | Working directory for new shells. |

### Remote access with Tailscale

```sh
tailscale serve --bg --https=8443 http://127.0.0.1:7789
```

Then use `https://<machine>.<tailnet>.ts.net:8443` as the terminal URL.

### Remote access with cloudflared

> **Warning:** a cloudflared tunnel is reachable from the public internet. Only the token protects the shell. Prefer Tailscale when you can.

**Quick tunnel** (no Cloudflare account):

```sh
cloudflared tunnel --url http://127.0.0.1:7789
```

Use the printed `https://<random>.trycloudflare.com` address as the terminal URL. A quick tunnel forwards a single port, so zot needs its own tunnel and gets a different URL. The address changes on every start.

**Named tunnel** (Cloudflare account and your own domain): route zot and zotty through one hostname by path:

```yaml
ingress:
  - hostname: zot.example.com
    path: ^/term
    service: http://127.0.0.1:7789
  - hostname: zot.example.com
    service: http://127.0.0.1:7787
  - service: http_status:404
```

Then use `https://zot.example.com` as the host URL and `https://zot.example.com/term` as the terminal URL.

## Behavior

- One shell per connection. Closing the tab or reconnecting ends that shell (use `tmux` inside it for persistent sessions).
- At most 8 shells at a time.
- The token is sent in the first WebSocket message, not in the URL, so it does not appear in tunnel or proxy logs.
- Browser origins other than localhost and `--origins` are rejected.
- Unix only (macOS, Linux).

## Protocol

WebSocket at `/term`:

| Direction | Frame | Content |
|---|---|---|
| client to server | text, first | `{"type":"auth","token":"...","cols":80,"rows":24}` |
| client to server | text | `{"type":"resize","cols":120,"rows":40}` |
| client to server | binary | terminal input |
| server to client | text | `{"type":"ready"}` after authentication |
| server to client | text | `{"type":"exit","code":0}` when the shell exits |
| server to client | binary | terminal output |

A wrong token closes the connection with status 1008, too many shells with 1013.

## Test

```sh
go test -race ./...
```

## License
MIT
