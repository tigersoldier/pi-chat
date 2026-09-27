# gateway-probe

Dev smoke test for the pi-chat ↔ pi-gateway seam: dials `pi-gatewayd` with the two
token files pi-chat uses, checks the capability split, runs one real turn on each
connection, and verifies that `gw_delete_session` unbinds every client and refuses
later session-scoped work.

It is a **nested module**, so the parent `go build ./...` ignores it. Run it from
this directory:

```sh
go run .        # needs pi-gatewayd running and the token files in place
```

Requires `~/.config/pi-chat/gateway-{admin,thread}.token` and the daemon on
`127.0.0.1:7331` (discovered through `~/.config/pi-gateway`). It leaves no session
behind: the session it creates is deleted at the end.
