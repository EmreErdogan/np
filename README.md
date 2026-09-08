# np

Share plain-text notes between the machines on your Tailscale tailnet.
Think of it as a tiny, opinionated git for notes: every machine has a full
copy, every version is kept, and sync works peer-to-peer or through a hub.

No accounts, no TLS setup, no tokens: the tailnet is the network and the
identity. `np serve` listens only on the machine's Tailscale IP and accepts
requests only from nodes that belong to the same Tailscale login (extend with
`allow` in config).

## Install

```sh
go build -o np .            # this machine
GOOS=darwin GOARCH=arm64 go build -o dist/np-darwin-arm64 .
```

Requires a running Tailscale on every machine.

## Use

```sh
np new todo                 # opens $EDITOR; or: echo "buy milk" | np new todo
np ls
np cat todo
np edit todo
np log todo                 # history with vector clocks
np show todo 2              # print version 2
np rm todo                  # deletion syncs as a tombstone

np peers                    # who is on the tailnet and who runs np
np hub laptop               # default sync target
np sync                     # sync with hub
np sync desktop              # or with any peer directly
np daemon                   # serve + auto-sync every 15s (run on the hub too)
```

Notes are markdown files in `~/.np/notes/` (override with `NP_DIR`). Edit
them with anything; np notices external changes on the next command.

## How sync works

- Each note carries a vector clock (`server:3,laptop:1`). Sync compares clocks:
  the side that is strictly ahead wins, and its version is copied over.
- Concurrent edits (neither side has seen the other's change) are resolved by
  newest modification time. The losing version is kept as
  `name.conflict-<node>-<time>.md` next to the winner, so nothing is lost.
  The merged result gets a fresh clock and propagates to every peer.
- Deletes are tombstones, so they propagate too.
- Every version of every note is stored under `~/.np/history/`.

## Config

`~/.np/config.json`:

```json
{ "hub": "laptop", "port": 7373, "interval_seconds": 15,
  "allow": ["friend@example.com"] }
```
