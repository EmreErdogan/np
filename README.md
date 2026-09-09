# np

Share plain-text notes, and any other files, between the machines on your
Tailscale tailnet. Think of it as a tiny, opinionated git for notes: every
machine has a full copy, every version is kept, concurrent edits are merged,
and sync works peer-to-peer or through a hub. Big files ride along lazily:
every machine knows about them, only the ones that ask download them.

No accounts, no TLS setup, no tokens: the tailnet is the network and the
identity. `np serve` listens only on the machine's Tailscale IP and accepts
requests only from nodes that belong to the same Tailscale login (extend with
`allow` in config).

## Install

One line, no sudo:

```sh
curl -fsSL https://raw.githubusercontent.com/EmreErdogan/np/main/install.sh | sh
```

It downloads the latest release for your OS and CPU, verifies the checksum,
puts `np` in `~/.local/bin` (override with `NP_INSTALL_DIR`) and adds that
directory to your shell's PATH if needed. Later, `np upgrade` fetches the
newest release in place and restarts the service if one is installed.

Alternatives: grab a binary from the
[releases page](https://github.com/EmreErdogan/np/releases), or
`go install github.com/EmreErdogan/np@latest`.

Requires a running Tailscale on every machine.

## Use

```sh
np new todo                 # opens $EDITOR; or: echo "buy milk" | np new todo
np ls                       # tree view with sync state against the hub: synced/ahead/new/behind/conflict
np ls work                  # only notes under work/
np ls --local               # skip the hub lookup; --flat for a plain list
np ls --path                # absolute paths instead of names (flat)
np path todo                # absolute path: vim $(np path todo), open $(np path pic.jpg)
np path -c todo             # ...copied to the clipboard (over SSH: your local one, via OSC 52)
np new work/ideas/next      # slashes make folders
np cat todo                 # raw content
np view todo                # rendered in the terminal
np new config.json          # non-markdown notes keep their extension
np search milk              # name and content, case-insensitive
np edit todo
np add todo "buy milk"      # append without opening the file
np add todo                 # ...from an empty editor buffer
pbpaste | np add links      # ...or from stdin
np add -t journal "call"    # prefix a timestamp
np log todo                 # history with vector clocks
np show todo 2              # print version 2
np rm todo                  # deletion syncs as a tombstone

np put ~/Pictures/cat.jpg   # share any file (name defaults to cat.jpg)
np put video.mp4 trips/     # ...into a folder
np get video.mp4            # download a file that only the hub/peers hold
np get video.mp4 -o ~/Desktop
np drop video.mp4           # free the local copy, keep knowing about it

np peers                    # who is on the tailnet and who runs np
np hub laptop               # default sync target
np sync                     # sync with hub
np sync desktop              # or with any peer directly
np sync --all               # every online peer that runs np
np daemon                   # serve + auto-sync every 15s (run on the hub too)
np service install          # run the daemon at login (systemd user / launchd)
np service restart          # e.g. after editing config.json
```

Run the daemon (or the service) on every machine. A machine that receives a
push fans it out to all other online np peers after a 2s debounce, so with a
hub every edit reaches every machine within a few seconds. Without a hub, or
while the hub is unreachable, the daemon syncs with every online np peer on
each tick instead, so a pure mesh converges too. Pick an always-on machine as
the hub and give it `keep_all` so it holds every file.

Notes are plain files in `~/.np/notes/` (override with `NP_DIR`). A name
without an extension is markdown and stored as `name.md`; names with a short
extension such as `config.json` or `deploy.sh` are stored as-is and shown as
highlighted code by `np view` and the web UI. Edit the files with anything;
np notices external changes on the next command.

### Appending

`np add` puts text at the end of a note and creates the note if needed. A
blank line is inserted before the addition so it renders as its own
paragraph, unless a list item is added to a list, which keeps the list tight.
The web UI has the same thing as an "add a line" box under every note.

### Files

Any file (images, video, archives) can be shared next to the notes. They
live in `~/.np/files/` and sync with the same clocks, tombstones and
conflict copies as notes, but with two differences: no version history, and
content is fetched lazily. Every machine learns that a file exists; its
bytes follow automatically only when the file is at most `auto_fetch_bytes`
(2 MB by default), when this machine already held an earlier version, or
when the node has `keep_all` set (do that on the hub, which then acts as
the archive). Bigger files are listed as "not fetched" until `np get`, or
the Fetch button in the web UI. `np ls` shows files in a block at the end.
`np rm` deletes a file when no note has that name; `np path` works for both.
Set `auto_fetch_bytes` to `-1` to never fetch automatically.

## Web UI

The daemon serves a small phone-friendly page at `http://<tailscale-ip>:7373/`
(the URL is shown by `np status`). Open it from any device on the tailnet,
such as a phone running Tailscale, to read, edit, create and delete notes.
Markdown notes are rendered; other file types are shown as highlighted code.
Every note has a History page listing its versions; any version can be
viewed and restored as the new current content. Files have their own list
with an upload box (photos from a phone, for instance); images, video,
audio and small text files preview inline, everything else downloads.
Edits are committed like local edits and fanned out to peers immediately.
Access uses the same tailnet identity rules as sync.

## How sync works

- Each note carries a vector clock (`server:3,laptop:1`). Sync compares clocks:
  the side that is strictly ahead wins, and its version is copied over.
- Concurrent edits (neither side has seen the other's change) of a text note
  are merged line by line against the last version both sides knew, like a
  git three-way merge. When both sides changed the same lines, or a file is
  binary, the newest modification time wins and the losing version is kept
  as `name.conflict-<node>-<time>.md` next to the winner, so nothing is
  lost. The merged result gets a fresh clock and propagates to every peer.
- Deletes are tombstones, so they propagate too.
- Every version of every note is stored under `~/.np/history/`.

## Config

`~/.np/config.json`:

```json
{ "hub": "laptop", "port": 7373, "interval_seconds": 15,
  "allow": ["friend@example.com"],
  "auto_fetch_bytes": 2097152, "keep_all": false }
```
