# Reflecting Pool — scope

A discoverability suite for a NAS: see where the space goes, find what is no longer needed, and record what things are for.

Agreed scope as of 2026-10-04. A change to this document is a change to scope.

## Target environment

- An Unraid server, with the tool deployed as a Docker container.
- Primary storage is ZFS pools (raidz2). The reference deployment has 72 TB of capacity and 24 TB used: about 14 TB of media and 10 TB of files and backups.
- Unraid parity arrays are supported by the same scanner but are secondary. They are tested on fixture trees only, not on a real array.

## Decisions

| Area | Decision |
|---|---|
| Language | Go; one static binary with the web UI embedded |
| Packaging | Docker container |
| Access | Built-in login over HTTPS |
| Index | SQLite in the container's appdata, one index per scan |
| Annotations | Plain-text files in `.reflection/` at the root of each share |
| File names | Never changed implicitly. Prefixes and display names live in metadata; a real rename is an explicit action |
| Deleting | Quarantine first, purge later |
| ZFS access | Optional and read-only. The tool never creates or destroys snapshots and never changes properties |
| Frontend | Presentation is decided by a design evaluation before any UI code is written |
| Licence | MIT |
| Units | Decimal (GB, TB) by default; a setting switches to binary (GiB, TiB) |
| Interface design | `design/PROPOSAL.md`, approved with the decisions recorded there |

## Features

### 1. Space breakdown

- Graphical breakdown of occupied space by pool, dataset or share, folder and file.
- On-disk size (after compression) and apparent size, switchable.
- Hardlinked files are counted once.
- Space held by snapshots is shown as its own figure per dataset (needs ZFS access; see "ZFS handling").
- Quarantined items are shown as their own category.
- Growth over time: folder totals are kept from each scan, and the tool shows what grew between scans.

### 2. Discovery

- Largest files and folders, with filtering and sorting by size, age, type, share, prefix and annotation status.
- Search by name and by annotation text.
- **Staleness**: an item's age comes from its modified time, its arrival (creation) time, and its read time where the dataset records one. A folder's age is the newest value anywhere beneath it. Which clocks count is configurable per dataset.
- **Duplicates**: an on-demand background job. It compares only files of identical size above a minimum size, by partial hash first and full hash for the matches that remain. Results are cached until a file changes. The tool reports duplicates; it never removes or links them automatically.
- **Orphans**:
  - files in download folders with a link count of 1, which the *arr apps never imported;
  - appdata folders with no matching container, judged from Unraid's saved container templates mapped read-only. The Docker socket is not used.

### 3. Annotations

- A free-text description of what a file or folder is for.
- Optional owner and review-after date.
- Stored as human-readable files under `<share>/.reflection/`, written atomically and readable without the tool.
- An annotation follows its item through renames, identified by dataset, inode and creation time. After a copy-style move it is re-attached by size, modified time and a partial hash.
- A coverage figure per share, and a generated `INDEX.md` per share.

### 4. Naming

- Detects poorly readable names: random strings, hashes, UUIDs, names with few vowels.
- Skips locations where such names are normal (appdata, system, `.git` and similar). The exclusions are configurable.
- Suggests a name from content metadata (EXIF, ID3, PDF title) and from the parent folder.
- The chosen name is stored as a display name in metadata by default. A real rename is offered as an explicit, logged, undoable action.

### 5. Prefixes

- A user-maintained list of prefixes, each with a meaning.
- Prefixes are assigned to files and folders as metadata tags. File names are unchanged.
- Filter, group and search by prefix.

### 6. Quarantine

- Removing an item moves it to a quarantine folder inside the same dataset. The move is instant and keeps hardlinks and ownership.
- An item can be restored at any time before it is purged. Purging happens after a configurable retention period: 30 days by default, or never.
- Every move, rename, restore and purge is recorded in an undo log.
- The tool warns when snapshots will keep the space from being freed.

### 7. Scanning

- A parallel, metadata-only walk of each pool. A scan never reads file contents.
- Scans run when the user starts one, including the first. A schedule is optional and off by default. All browsing is served from the index.
- Progress is a true percentage, from the per-dataset file count ZFS reports.
- Excluded by default: `.zfs` directories and Docker's per-layer datasets.
- A benchmark and diagnostics command.
- Scan intensity: before starting a scan by hand, the user chooses aggressive, balanced or low impact. The dialog preselects balanced, then the last choice; a scheduled scan runs at the intensity set with its schedule. The gentler modes use fewer walkers at lower priority and rest between reads in proportion to how long the reads took, so they back off when the disks are busy. Their effect on a real pool is still to be measured ([issue #6](https://github.com/IsaacFW/reflectingpool/issues/6)).

### 8. Security

- One admin account, created at first start with a one-time setup code printed to the container log.
- Passwords hashed with argon2id; HttpOnly, SameSite=Strict session cookie; CSRF token on every state-changing request; rate-limited login.
- HTTPS by default, with a self-signed certificate generated on first run or a supplied one. Plain HTTP only by explicit setting, for use behind a reverse proxy.
- The container runs unprivileged: "Privileged" off, no added capabilities.
- A read-only mode that disables every action that changes files.

### 9. Review workflow

Working through many items to annotate or rename them has to be fast and easy. This is the main way the user fills in a share.

- **One item at a time**: the review screen shows the current item's fields, with a preview of the item in a sidebar on the right side of the screen.
- **Preview**: a best-effort preview of whatever the item is. Images, video, audio, PDF and text are shown directly, a folder as a listing of its contents, and anything else as its metadata.
- **Keyboard**: Tab moves to the next field. Ctrl+Enter confirms and moves to the next item; on a blank form it skips the item. Ctrl+Z goes back to the previous item once there is no typing left to undo.
- **Folder-level tools**: describe the containing folder instead of the item, leave out items inside folders that are already described, and skip the rest of a group. A million files cannot be reviewed one at a time.
- **Generated previews**: video the browser cannot play gets a poster frame, and RAW and HEIC photos get a thumbnail, in the first release.
- **Review order**: the user builds the order from a list of sort keys, such as share, file type, size, age and folder. Example: share, then file type, largest first. The user works through one share, taking each file type in turn with its largest files first. When every item of that type is filled in or skipped, the queue moves to the next file type, and after the last type to the next share.
- **Progress**: filled-in and skipped items are remembered in `.reflection/`, so a review can be stopped and resumed. The screen shows how many items remain in the current group.
- **Skips** are kept in their own list, `skipped.jsonl`, one path per line, apart from the annotations. Skipping the rest of a group can add tens of thousands at once, and a skip records nothing worth following: a skipped item that is renamed or moved returns to review.
- **A described folder** covers what is inside it only when it has a note, a prefix or a display name. A skipped folder does not, and neither does a note on a share.

## ZFS handling

- Each dataset is treated as a separate filesystem. Inode identity, hardlinks and quarantine folders are all per dataset.
- `/dev/zfs` may be passed to the container for dataset, compression and snapshot accounting. The tool runs list and get commands only.
- Without `/dev/zfs`, everything works except snapshot sizes. A scheduled host script that writes `zfs list` output to a file is the supported alternative.

## Container configuration

| Mapping | Mode | Needed for |
|---|---|---|
| `/mnt/<pool>` (one per pool) | Read-write, slave propagation | Scanning, annotations, quarantine |
| appdata folder | Read-write | Index, configuration, undo log |
| `/dev/zfs` device | Optional | Snapshot and compression accounting |
| Unraid container templates folder | Read-only, optional | Appdata orphan detection |

## Performance targets

For the reference deployment, to be confirmed by benchmark:

- Full scan with a warm cache: under 2 minutes.
- Full scan with a cold cache: under 30 minutes.
- Any browsing query: under 100 ms.
- One step of a review (record an item, fetch the next): a few milliseconds, however many items have been reviewed or skipped.
- Memory during a scan: under 500 MB.

## Build order

1. **Backend core**: scanner, index, ZFS reader, login, annotations and prefixes, review queue and preview endpoints, JSON API, benchmark command, Docker image.
2. **Design evaluation**: a frontend design agent works from real scan data and the user tasks; its proposal is approved before UI work starts.
3. **First usable release**: UI for space breakdown, discovery, annotations, prefixes and the review workflow.
4. **Remaining features**, each as backend plus UI: staleness, naming, quarantine, orphans, growth, duplicates.
5. **Unraid packaging**: Community Applications template and setup documentation.

## Verification

- Automated tests on fixture trees for the scanner, index, annotations, quarantine and login.
- ZFS behaviour is verified on the reference server through the diagnostics command; the development machine has no ZFS.
- Confirmed on the reference server on 2026-10-04 (Unraid 7.3.2, OpenZFS 2.4.3 with the image's 2.4.4 tools):
  - an unprivileged container with `/dev/zfs` lists datasets and is refused when it tries to create a snapshot;
  - a share mapped on its own is recognised as its ZFS dataset;
  - the scan's on-disk total matches ZFS's own figure for the dataset, and its entry count matches ZFS's object count to within 0.1%;
  - a whole pool of about 1.1 million entries across 13 datasets scanned in 1 to 4 minutes with 357 MiB peak memory, measured while a large transfer was writing to the pool, so an idle pool should be no slower. Every measured query is within its target except name search at 130 to 145 ms ([issue #7](https://github.com/IsaacFW/reflectingpool/issues/7)).

- Measured on the development machine on 2026-10-05, on a generated tree of 1 million files in 137,000 folders (not yet on the reference server):
  - one step of a review takes about 1 ms in every queue shape tried, including after 274,000 items had been skipped at once and with a described folder left out;
  - the first call of a new queue takes 0.1 to 0.9 s, and changing to a group that no index leads to about 0.15 s;
  - skipping 274,000 items at once takes about 2.3 s; 200,000 skips add about 0.8 s to a scan and to start-up.

## Out of scope

- Unraid plugin packaging.
- Incremental rescans through `zfs diff`, and any other ZFS write.
- Multiple user accounts or roles.
- Automatic deduplication (hardlinking or deleting duplicates).
- Looking inside archives or backup images.
- Media-server integration, such as last-played data.
- Phone layouts, for now.
- Renaming, moving or deleting anything without an explicit user action, apart from the quarantine purge after its retention period.

## Open items

- Step 3 is under way: the shell (sign-in, scans, settings) and the Space list with its inspector are built. Still to build: Review, Find, the map, Overview, Annotations and Storage.
- The interface has been run in headless Chromium only. Firefox and Safari, and PDF and video previews as a person sees them, are unchecked.
- Still to be checked on the reference server: datasets nested inside a share, snapshot accounting with real snapshots, saving annotations, and the review timings that `bench` now reports.
