# Reflecting Pool

A discoverability suite for a NAS: see where the space goes, find what is no longer needed, and record what things are for. Built for Unraid servers that use ZFS pools; [SCOPE.md](SCOPE.md) has the full scope.

**Status: backend only.** The scanner, index, login, annotations, review queue and JSON API work and are tested. There is no web interface yet; that follows a design evaluation (see "Build order" in the scope). Until then the container is useful for checking that it works on your server and for measuring scan speed.

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

## Check it on your server

Three commands, none of which change anything on the pool:

```sh
docker exec reflectingpool reflectingpool doctor   # mappings, permissions, ZFS access
docker exec reflectingpool reflectingpool bench    # scan and query speed on your data
docker exec reflectingpool zfs snapshot tank/SOME-SHARE@rp-permission-test
```

The third must **fail** with "permission denied". It confirms that an unprivileged container cannot change ZFS even with `/dev/zfs` passed in. If it succeeds, remove the snapshot on the host (`zfs destroy tank/SOME-SHARE@rp-permission-test`), stop passing `/dev/zfs`, and use the host script below instead. This behaviour is expected from how OpenZFS checks permissions but has not been verified on Unraid.

`bench` walks the tree twice, then runs a full scan into a throwaway index and times typical queries. The first walk shows a cold cache if the pool has been idle.

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
| `RP_SCAN_INTERVAL` | `24h` | Time between scheduled scans; `0` disables them |
| `RP_WORKERS` | 4 per core, 8 to 32 | Parallel directory walkers |
| `RP_ZFS_LIST_FILE` | `<RP_DATA>/zfs-list.txt` | Output of the host script above |

Always skipped: `.zfs` snapshot directories, `<root>/system/docker` (Docker's image layers), each share's `.reflection` folder, and the data directory itself.

## What it writes

- **In each share:** a `.reflection/` folder, created the first time something in that share is annotated. `annotations.jsonl` holds one line per annotated item and is the only copy of that information; `INDEX.md` is a readable summary regenerated from it. Both take the share's owner and permissions.
- **In the data directory:** the scan index (rebuilt by every scan; the last three are kept), the app database and the TLS key. All of it can be deleted without losing annotations.
- **Nowhere else.** A scan reads metadata only and never opens a file. File contents are read in two cases: a 128 KiB fingerprint when you annotate a file, and when you preview one.

## Security

- **Keep "Privileged" off.** The program needs to read every file, so it runs as root inside the container, and the pool is mapped read-write for annotations. With privileges, the same process could also destroy datasets and snapshots. Without them, your snapshots stay out of its reach.
- **Login.** One admin account. Passwords are hashed with Argon2id; sessions use an HttpOnly, SameSite=Strict cookie plus a CSRF token on every change. Repeated failures from one address are locked out for 30 seconds, doubling up to 15 minutes.
- **Forgotten password.** `docker exec reflectingpool reflectingpool reset-admin` deletes the account; a new setup code then appears in the log.
- **Previews.** Files on a share are untrusted. HTML, XML and scripts are served as plain text and SVG is sandboxed, so a file cannot run as a page of this site.

## API

Everything but the first three routes needs the session cookie; anything but `GET` also needs the `X-CSRF-Token` header returned by login.

| Route | Purpose |
|---|---|
| `GET /api/health` | Liveness, and whether setup is still needed |
| `POST /api/setup` | Create the admin account with the setup code |
| `POST /api/login`, `POST /api/logout`, `GET /api/session` | Sessions |
| `GET /api/scan`, `POST /api/scan` | Scan status and progress; start a scan |
| `GET /api/storage` | Datasets with usage and, where available, snapshot accounting |
| `GET /api/tree?id=` | One folder's children with rolled-up sizes, for the space breakdown |
| `GET /api/entries?…` | Find files and folders by kind, share, type, size, age, name, annotation state or prefix |
| `GET /api/entries/{id}` | One item with its annotation and hardlinks |
| `PUT`, `DELETE /api/entries/{id}/annotation` | Record or remove an annotation; `{"skipped": true}` passes an item over |
| `GET /api/entries/{id}/content` | File contents for the preview pane, with range requests |
| `GET /api/shares`, `GET /api/shares/{id}/annotations` | Annotation coverage per share; everything recorded in one |
| `GET`, `PUT /api/prefixes` | The prefix list |
| `POST /api/queue` | The next items of a review, grouped and ordered by the keys you give |

A review that goes share by share, then file type, largest first:

```json
{"filter": {"kind": "file"}, "groups": ["share", "type"], "order": "size"}
```

Groups are served largest first and one at a time; an item leaves the queue when it is annotated or skipped.

## Development

Requires Go 1.27 or Docker.

```sh
go test ./...                                      # unit and API tests
go test -race ./...                                # needs cgo
go run ./cmd/reflectingpool bench /some/directory  # speed on any tree
```

The tests run on any Linux filesystem. Nothing in them needs ZFS, which also means the ZFS-specific paths (`zfs list` through `/dev/zfs`, dataset boundaries, snapshot accounting) are covered by parsers and fixtures only until checked on a real pool.

## Licence

[MIT](LICENSE).
