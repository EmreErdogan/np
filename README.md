# np

Share notes, and any other files, between the machines on your Tailscale
tailnet. Think of it as a tiny, opinionated git for a folder: every machine
knows every file, every version of a text file is kept, concurrent edits
are merged, and sync works peer-to-peer or through a hub. Big binary files
ride along lazily: every machine knows about them, only the ones that ask
download them.

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
newest release in place and restarts the service if one is installed. Like
the installer it reads the release page, not the GitHub API, so it needs no
token and is not subject to the API's hourly quota.

The service starts the np it was installed from. If that is another copy
than the one on your PATH, upgrading does not reach it: `np upgrade` and
`np service status` say so, and `np service install` points the service at
the np you are running.

Alternatives: grab a binary from the
[releases page](https://github.com/EmreErdogan/np/releases), or
`go install github.com/EmreErdogan/np@latest`.

Requires a running Tailscale on every machine.

## Use

```sh
np new todo                 # opens $EDITOR on todo.md; or: echo "buy milk" | np new todo
np ls                       # tree view with sync state against the hub: synced/ahead/new/behind/conflict
np ls work                  # only what is under work/
np ls --local               # skip the hub lookup; --flat for a plain list
np ls --path                # absolute paths instead of names (flat)
np path todo                # absolute path: vim $(np path todo), open $(np path pic.jpg)
np path -c todo             # ...copied to the clipboard (over SSH: your local one, via OSC 52)
np new work/ideas/next      # slashes make folders
np cat todo                 # raw content
np view todo                # rendered in the terminal
np new config.json          # an extension picks the type; without one it is markdown
np search milk              # names and text content, case-insensitive
np edit todo
np add todo "buy milk"      # append without opening the file
np add todo                 # ...from an empty editor buffer
pbpaste | np add links      # ...or from stdin
np add -t journal "call"    # prefix a timestamp
np log todo                 # history with vector clocks
np show todo 2              # print version 2
np diff todo                # what the latest version changed; np diff todo 3 (v2->v3); np diff todo 2 5
np rm todo                  # deletion syncs as a tombstone
np mv todo work/            # rename or move; history and identity follow

np put ~/Pictures/cat.jpg   # copy any file in (name defaults to cat.jpg)
np put video.mp4 trips/     # ...into a folder
np put -f cat.jpg           # replace an existing file
np get video.mp4            # download content that only the hub/peers hold
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

### One folder, real names

Everything lives in `~/.np/files/` (override with `NP_DIR`) under its real
name: `todo.md`, `config.json`, `trips/cat.jpg`. There is one namespace, so
a name means one thing in every command and on every page. `todo` is a
shorthand for `todo.md` wherever a name is typed; a new file without an
extension becomes markdown. Edit the files with anything; np notices
external changes on the next command.

What a file can do follows from its content, not from how it got in:

- **Text up to 1 MB** (markdown, code, config, plain text) keeps every
  version, is rendered and editable in the web UI, shows up in `np search`,
  merges concurrent edits and is copied to every machine.
- **Everything else** (images, video, archives, larger files) is a blob: no
  version history, the newest change wins a conflict and the other copy is
  kept next to it, and content is fetched lazily. Every machine learns that
  the file exists; its bytes follow automatically only when it is at most
  `auto_fetch_bytes` (2 MB by default), when this machine already held an
  earlier version, or when the node has `keep_all` set (do that on the hub,
  which then acts as the archive). The rest is listed as "not fetched"
  until `np get`, or the Fetch button in the web UI. Set `auto_fetch_bytes`
  to `-1` to never fetch automatically.

### Appending

`np add` puts text at the end of a text file and creates it if needed. A
blank line is inserted before the addition so it renders as its own
paragraph, unless a list item is added to a list, which keeps the list tight.
The web UI has the same thing as an "add a line" box under every text file.

### Upgrading from 0.14 or older

Older versions kept notes in `~/.np/notes/` and other files in
`~/.np/files/`, as two namespaces. The first command of the new version
copies the notes in next to the files (`todo` becomes `files/todo.md`).
Nothing is deleted or overwritten on the way: no file under `files/`
moves, the original notes stay in `~/.np/notes.v1/` and the previous index
in `index.v1.json` (remove both once you are happy). Where a note and a
file would get the same name, the file keeps it and the note becomes
`name.note.md`; np says so when it happens. Text that used to be a plain
file had no history; it gets a snapshot of its current content on that
first run and keeps every version from then on. The sync protocol changed
with the layout: machines sync again once all of them are upgraded, so
sync first, then run `np upgrade` everywhere. np will not convert the store
while a daemon of an older version is still running on the machine; it
tells you to run `np service install` (or to stop that daemon) first.

## Web UI

The daemon serves a small phone-friendly page at `http://<tailscale-ip>:7373/`
(the URL is shown by `np status`). Open it from any device on the tailnet,
such as a phone running Tailscale. It shows one folder at a time, with the
most recently changed files on the first page; the box at the top filters
the page as you type and, on Enter, searches names and text content
everywhere. Text files are rendered (markdown, or highlighted code) and can
be edited, appended to and restored from their History page, which lists
every version with the size of each change and shows the diff from the
previous one. Images, video and audio preview inline, everything else
downloads. Files can be uploaded (photos from a phone, for instance; a
markdown file uploaded this way is a text file like any other), renamed
and deleted from their pages. Edits are committed like local edits and
fanned out to peers immediately. Access uses the same tailnet identity
rules as sync.

## How sync works

- Each file carries a vector clock (`server:3,laptop:1`). Sync compares clocks:
  the side that is strictly ahead wins, and its version is copied over.
- Concurrent edits (neither side has seen the other's change) of a text file
  are merged line by line against the last version both sides knew, like a
  git three-way merge. When both sides changed the same lines, or the file
  is a blob, the newest modification time wins and the losing version is
  kept as `name.conflict-<node>-<time>.md` next to the winner, so nothing
  is lost. The merged result gets a fresh clock and propagates to every peer.
- Deletes are tombstones, so they propagate too. A rename is a new record
  that inherits the old clock and ledger plus a forward-pointing tombstone,
  so `np log` continues across the rename on every machine.
- Every version of every text file is stored under `~/.np/history/`, and
  sync exchanges the versions a machine is missing, so history is the same
  on every machine. Blobs have a ledger too (who changed it, when, how
  big), without the content.

## Config

`~/.np/config.json`:

```json
{ "hub": "laptop", "port": 7373, "interval_seconds": 15,
  "allow": ["friend@example.com"],
  "auto_fetch_bytes": 2097152, "keep_all": false }
```
