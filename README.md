# Reflecting Pool

A discoverability suite for a NAS: see where the space goes, find what is no longer needed, and record what things are for. Built for Unraid servers that use ZFS pools; [SCOPE.md](SCOPE.md) has the full scope.

**Status: the first release's screens are built.** From a browser you can scan the pool, see where the space goes (as a list and as a map), search everything, work through a review queue one item at a time, manage prefixes and see how much of each share is described, and look at the datasets behind it all. Films in any format ffmpeg reads are shown as a sheet of stills, and RAW and HEIC photos as pictures. Still to come: the step 4 features (quarantine, staleness, naming, duplicates).

## Run it on Unraid

Every push to `main` tests the code and publishes the image to GitHub's container registry:

```
ghcr.io/isaacfw/reflectingpool:latest
```

Add a container with these settings. `tank` stands for your pool's name.

| Setting | Value | Why |
|---|---|---|
| Repository | `ghcr.io/isaacfw/reflectingpool:latest` | Each build also gets a fixed tag, `main-<commit>`, if you prefer to pin one |
| Path | `/mnt/tank` to `/mnt/tank`, access mode **Read/Write - Slave** | Scanning, annotations. "Slave" lets datasets created later show up without a restart |
| Path | `/mnt/tank/appdata/reflectingpool` to `/data` | The index, the admin account, the TLS key |
| Variable | `RP_ROOTS=/mnt/tank` | What to scan. Several pools: comma-separated, one path mapping each |
| Port | `8443` | HTTPS |
| Device | `/dev/zfs` (optional) | Snapshot and compression accounting |
| Privileged | **off** | See "Security" |

The same thing as a command:

```sh
docker run -d --name reflectingpool \
  -p 8443:8443 \
  -v /mnt/tank:/mnt/tank:rw,slave \
  -v /mnt/tank/appdata/reflectingpool:/data \
  --device /dev/zfs \
  -e RP_ROOTS=/mnt/tank \
  ghcr.io/isaacfw/reflectingpool:latest
```

### Only some shares

To give the container one share instead of the whole pool, map the share into a folder of its own and point `RP_ROOTS` at that folder:

| Setting | Value |
|---|---|
| Path | `/mnt/tank/media` to `/pool/media`, access mode **Read/Write - Slave** |
| Variable | `RP_ROOTS=/pool` |

Every folder mapped into `/pool` is scanned as a share. Annotations record paths relative to their share, so they stay valid if you later map the whole pool instead.

Map from `/mnt/<pool>/...`, not `/mnt/user/...`: the user-share layer hides which dataset a file is on and is slower to walk.

### First start

On first start the container log shows two things you need:

- **A setup code.** Creating the admin account requires it, so nobody else on the network can claim the server first.
- **The certificate's SHA-256 fingerprint.** The certificate is self-signed, so the browser will warn once; the fingerprint lets you confirm the certificate it shows is this one.

Then open `https://<server>:8443` in a browser, accept the certificate once, and create the account with the setup code.

Nothing is scanned until you ask. **Start the first scan** opens a dialog with three intensities; Balanced is a sensible first choice. The page shows the scan's progress and has a Stop button, and you can set a daily scan under Settings. The server never scans by itself otherwise.

A scan can also be run from the command line; a running server picks the result up within half a minute:

```sh
docker exec reflectingpool reflectingpool scan -intensity balanced
```

## Check it on your server

Three commands, none of which change anything on the pool:

```sh
docker exec reflectingpool reflectingpool doctor   # mappings, permissions, ZFS access
docker exec reflectingpool reflectingpool bench    # scan and query speed on your data
docker exec reflectingpool zfs snapshot tank/SOME-SHARE@rp-permission-test
```

The third must **fail** with "permission denied". It confirms that an unprivileged container cannot change ZFS even with `/dev/zfs` passed in. If it succeeds, remove the snapshot on the host (`zfs destroy tank/SOME-SHARE@rp-permission-test`), stop passing `/dev/zfs`, and use the host script below instead. This behaviour is expected from how OpenZFS checks permissions but has not been verified on Unraid.

`bench` walks the tree twice, then runs a full scan into a throwaway index and times typical queries. The first walk shows a cold cache if the pool has been idle.

### Scan intensity

A scan can run at three intensities, which trade speed for how much they disturb other work on the same disks:

| Intensity | What it does | When it is used |
|---|---|---|
| `aggressive` | 32 walkers at normal priority. Fastest; on a busy pool it takes about half the disks' attention | When asked for, and when a scan is started without choosing |
| `balanced` | 4 walkers at reduced priority, resting half as long as each read took | When asked for |
| `low` | 1 walker at the lowest priority, resting three times as long as each read took, so it backs off further when the disks are busy | When asked for; the suggested setting for a schedule |

A daily scan at a set time and intensity can be switched on through `PUT /api/settings`. It is off by default. If the server is not running at that time, the scan is skipped for the day, not started late.

To see what an intensity costs other work on your pool, start something that uses the disks, note its speed, and run a timed walk beside it:

```sh
docker exec reflectingpool reflectingpool bench -intensity low -for 5m
```

It walks for five minutes, reports the rate, and estimates how long a full scan would take at that intensity.

## ZFS

- **What is read.** Datasets under each root are found from the mount table and need nothing special. With `/dev/zfs` passed in, the program also runs `zfs list`, the only ZFS command it ever runs, to learn how much space snapshots hold.
- **Matching versions.** The `zfs` tool in the image must be the same release series as the host's ZFS module. The default image carries ZFS 2.4.x, which matches Unraid 7.3 ([7.3.2 ships OpenZFS 2.4.3](https://docs.unraid.net/unraid-os/release-notes/7.3.2/)). For an older host, build the image yourself with `docker build --build-arg ALPINE_VERSION=3.22 .` (ZFS 2.3.x). `doctor` prints both versions.
- **Without `/dev/zfs`.** Everything works except snapshot sizes. To get those without the device, schedule this on the host (for example hourly with the User Scripts plugin):

  ```sh
  out=/mnt/tank/appdata/reflectingpool/zfs-list.txt
  zfs list -Hp -t filesystem,volume,snapshot \
    -o name,type,used,avail,refer,usedsnap,usedds,usedchild,compressratio,mountpoint,atime,relatime,creation \
    > "$out.new" && mv "$out.new" "$out"
  ```

## Settings

All settings are environment variables.

| Variable | Default | Meaning |
|---|---|---|
| `RP_ROOTS` | (required) | Directories to scan, comma-separated |
| `RP_DATA` | `/data` | Where the index, account and TLS key are kept |
| `RP_LISTEN` | `:8443` | Address to listen on |
| `RP_TLS_CERT`, `RP_TLS_KEY` | | Your own certificate, replacing the self-signed one |
| `RP_INSECURE_HTTP` | off | `1` serves plain HTTP. Only behind a reverse proxy that adds HTTPS |
| `RP_TRUST_PROXY` | off | `1` trusts `X-Forwarded-For` and `X-Forwarded-Proto` from that proxy |
| `RP_READ_ONLY` | off | `1` disables everything that changes files, annotations included |
| `RP_EXCLUDE` | | Extra directories to skip, comma-separated absolute paths |
| `RP_WORKERS` | 4 per core, 8 to 32 | Parallel directory walkers for an aggressive scan |
| `RP_WEB_DIR` | built-in files | Serve the interface from this folder instead, for working on it |
| `RP_ZFS_LIST_FILE` | `<RP_DATA>/zfs-list.txt` | Output of the host script above |

Always skipped: `.zfs` snapshot directories, `<root>/system/docker` (Docker's image layers), each share's `.reflection` folder, and the data directory itself.

## What it writes

- **In each share:** a `.reflection/` folder, created the first time something in that share is annotated or skipped. `annotations.jsonl` holds one line per annotated item and is the only copy of that information; `INDEX.md` is a readable summary regenerated from it. `skipped.jsonl` lists the items passed over in a review, one path per line; a skipped item that is renamed or moved returns to review, and deleting the file returns them all at the next scan. All of it takes the share's owner and permissions.
- **In the data directory:** the scan index (rebuilt by every scan; the last three are kept), the app database, the TLS key, and generated previews (up to 1 GB; the ones used longest ago are cleared out first). All of it can be deleted without losing annotations.
- **Nowhere else.** A scan reads metadata only and never opens a file. File contents are read in two cases: a 128 KiB fingerprint when you annotate a file, and when you preview one.
- **Previews are made when you look at an item**, never during a scan, at most two at a time and at the lowest priority.

## Security

- **Keep "Privileged" off.** The program needs to read every file, so it runs as root inside the container, and the pool is mapped read-write for annotations. With privileges, the same process could also destroy datasets and snapshots. Without them, your snapshots stay out of its reach.
- **Login.** One admin account. Passwords are hashed with Argon2id; sessions use an HttpOnly, SameSite=Strict cookie plus a CSRF token on every change. Repeated failures from one address are locked out for 30 seconds, doubling up to 15 minutes.
- **Forgotten password.** `docker exec reflectingpool reflectingpool reset-admin` deletes the account; a new setup code then appears in the log.
- **Previews.** Files on a share are untrusted. HTML, XML and scripts are served as plain text and SVG is sandboxed, so a file cannot run as a page of this site.
- **Generated previews.** Films and HEIC photos are decoded by ffmpeg, which runs as `nobody`, not root. It is handed the one file as an open descriptor and allowed no other input, so a crafted file cannot make it read anything else; it has a time and a memory limit. RAW photos are not decoded: the JPEG the camera embedded is copied out. A bug in a decoder could still run code inside the container as `nobody`.

## API

Everything but the first three routes needs the session cookie; anything but `GET` also needs the `X-CSRF-Token` header returned by login.

**Entry IDs belong to one index.** Every scan builds a new index and numbers its entries afresh, so an ID kept from before a scan can come to mean a different item. Each response names the index in use in an `X-RP-Index` header. Send that value back in the same header:

- a request that changes something by entry ID is refused without it (`400`, code `index_required`);
- any request that names an index which has since been replaced is refused (`409`, code `index_changed`). Find the item again with `GET /api/entries/lookup?path=`; paths outlive scans.

**Errors** are `{"error": "message for a person", "code": "stable_code"}`. Act on the code.

**A folder row carries `types`**: what is inside it at any depth, by kind of file, as `{"type", "size", "disk", "files"}` for each kind. It is worked out from the index after each scan.

**Saving a note or a skip is refused with `403`, code `share_read_only`,** when the share cannot be written to, which on Unraid usually means the pool's path is mapped read-only.

| Route | Purpose |
|---|---|
| `GET /api/health` | Liveness, and whether setup is still needed |
| `POST /api/setup` | Create the admin account with the setup code |
| `POST /api/login`, `POST /api/logout`, `GET /api/session` | Sessions |
| `GET /api/scan`, `POST /api/scan`, `DELETE /api/scan` | Scan status, progress, schedule and recent scans; start a scan, optionally with `{"intensity": "aggressive" \| "balanced" \| "low"}`; stop the running scan |
| `GET`, `PUT /api/settings` | The optional daily scan: on or off, time of day, intensity |
| `GET /api/entries/lookup?path=` | The entry at a path |
| `GET /api/storage` | Datasets with usage and, where available, snapshot accounting |
| `GET /api/tree?id=` | One folder's children with rolled-up sizes, for the space breakdown. `kind=dir` gives the folders only, for the folder tree |
| `GET /api/entries?…` | Find files and folders by kind, share, type, size, age, name, annotation state or prefix, or everything `under=` a folder; `sort=` size, disk, name, modified, created, age or files. A `name` of several words finds names holding all of them, in any order, and `sort=match` then puts the closest names first |
| `GET /api/entries/{id}` | One item with its annotation and hardlinks |
| `PUT`, `DELETE /api/entries/{id}/annotation` | Record or remove an annotation. Saving with every field empty skips the item; deleting returns it to review |
| `GET /api/entries/{id}/content` | File contents for the preview pane, with range requests |
| `GET /api/entries/{id}/preview` | A picture made of a file the browser cannot show: stills from a film, a JPEG from a RAW or HEIC photo. `?info=1` gives the film's length, size and codec |
| `GET /api/shares`, `GET /api/shares/{id}/annotations` | Annotation coverage per share, and whether it is `writable`; everything recorded in one |
| `DELETE /api/shares/{id}/annotations?path=` | Remove a note whose item was not found at the last scan |
| `GET`, `PUT /api/prefixes` | The prefix list; reading it also gives how many items carry each prefix |
| `POST /api/queue` | The next items of a review, grouped and ordered by the keys you give |
| `POST /api/queue/skip` | Skip everything left in one group of a review |

### Review queues

A review that goes share by share, then file type, largest first:

```json
{"filter": {"kind": "file"}, "groups": ["share", "type"], "order": "size"}
```

Send the same request after each item is annotated or skipped: the item drops out and the answer holds the next ones. A group is served until nothing in it is left, then the next group starts.

| Field | Meaning |
|---|---|
| `filter` | Which items are candidates: `kind`, `shares`, `types`, `exts`, `min_size`, `max_size`, `modified_before`, `name`, and `under`, the ID of a folder whose contents (at any depth) are the only candidates |
| `groups` | Any of `share`, `type`, `ext`, `folder`, `age`, outermost first. Groups go in order of the bytes they hold, largest first. Age groups go oldest first: over 5 years, 2 to 5 years, 1 to 2 years, 6 to 12 months, under 6 months |
| `order` | Inside a group: `size` (largest first), `age`, `modified` or `created` (oldest first), `name`. `"desc": true` or `false` overrides the direction |
| `exclude_covered` | Count an item as reviewed when a folder above it has been described |
| `limit`, `offset` | How many items to return (20, at most 200), and how many unreviewed items to pass over first |

The answer gives `items`; `groups`, the groups still to do, starting with the current one; `group_position` and `group_total` ("group 3 of 31", finished groups included); `remaining`; and `total` and `total_size` for the whole queue.

- **Age** is counted from the later of an item's creation and last modification, up to the time of the scan. A file copied to the pool last week is a week old.
- **A described folder** is one with a note, a prefix or a display name. Skipping a folder says nothing about its contents, and a note on a share does not cover the share.
- **Only items inside a share** are offered. The scan roots, anything loose in them and the `.reflection` folders cannot be annotated.
- **To skip a group**, send the queue to `POST /api/queue/skip` with one more field, `group`: the `value` of each of the group's `values`, in order. The answer is `{"skipped": n}`.

## Development

Requires Go 1.27, or only Docker: `scripts/go.sh` runs the Go toolchain in a container, as in `scripts/go.sh go test ./...`.

```sh
go test ./...                                      # unit and API tests
go test -race ./...                                # needs cgo
go run ./cmd/reflectingpool bench /some/directory  # speed on any tree
scripts/typecheck.sh                               # the interface's types (Docker)
scripts/e2e.sh                                     # the interface in a real browser (Docker)
```

The tests run on any Linux filesystem. Nothing in them needs ZFS, which also means the ZFS-specific paths (`zfs list` through `/dev/zfs`, dataset boundaries, snapshot accounting) are covered by parsers and fixtures only until checked on a real pool.

### The interface

The interface is the `web/` folder: one page, one stylesheet, and plain JavaScript modules. There is no build step. The binary carries the files as they are, and what is in the folder is what runs.

- **Working on it:** set `RP_WEB_DIR` to the `web/` folder and the server reads the files from disk, so a change shows on the next reload.
- **Libraries:** Preact, htm and Preact Signals, about 26 kB in all, are copied into `web/lib/` at pinned versions. `web/lib/update.sh` fetches them, checks them against the checksums npm publishes, and records the result; a test fails if a file in that folder is changed any other way.
- **Rules a test enforces:** nothing inline in the page (it is served under a content security policy that allows scripts and styles from this server only), no import by package name, and no API that parses text as markup. File names and notes are untrusted text.
- **Types** are JSDoc comments, checked by `scripts/typecheck.sh`. It reads the files and builds nothing.
- **Browser tests** (`e2e/`, its own Go module) build the program, start it on a small tree and drive it through headless Chromium: setup, a scan, browsing, previews, saving a note, the keyboard, settings, signing out and in. They fail on any script error or anything the browser refuses. `RP_E2E_SHOTS=<folder>` saves a screenshot of every step.

## Licence

[MIT](LICENSE).
