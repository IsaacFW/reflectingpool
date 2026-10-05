# Reflecting Pool: interface proposal

Status: draft for the owner's approval. Nothing here is final code. The clickable mockup is `design/mockups/index.html`.

## Summary

1. **One layout rule for the whole product.** Navigation sits in a left rail, the work area is in the middle, and the right-hand column always shows "the item": an inspector while browsing, the preview while reviewing. The same preview and annotation components are used everywhere.
2. **Space breakdown: a sorted bar list is the primary form, with a treemap of the current folder above it.** The list answers "what is biggest here, exactly" and scales to a folder with a million children. The treemap answers "is there something large hidden two levels down". Sunburst and a treemap-only view are ruled out. An icicle is the close alternative to the treemap, and the mockup lets you switch between them.
3. **Review: three columns.** Queue progress on the left, the fields in the middle, the preview on the right. Tab moves between fields, Ctrl+Enter saves and advances, **PageDown skips**, PageUp goes back. Saving never blocks: the next item appears at once and the write finishes in the background.
4. **Review at this scale needs folder-level tools.** The pool holds about a million files. The design keeps the owner's file-by-file order and adds three things: "describe the folder instead" (Alt+Up), leaving out items inside folders that are already described, and "skip the rest of this group". Two of these need backend changes.
5. **Colour is reserved for data.** Three colour-blind-safe hues mark what kind of data a block holds (video, archives and disk images, images) with a neutral for everything else. A second mode colours by age on a single-hue scale. Snapshot and quarantine space are hatched, not coloured.
6. **Scans start from a dialog with three intensities** (low impact, balanced, aggressive), each described by how long it takes and what it does to the server. Balanced is preselected the first time and the last choice is remembered. Scheduled scans use a mode set in Settings; low impact is the proposed default. The mode in use shows next to the progress bar.
7. **Stack: Preact with htm and signals, vendored as a few small files, plain CSS, no build step and no Node.** The Go binary embeds the files as they are. This meets the strict CSP and the offline rule with nothing to install.
8. **The most important backend gap is index identity.** Entry IDs are reassigned by every scan. A browser that holds an ID across a scan could annotate the wrong item. The API needs an index generation on every response and a check on every write. The full gap list is in "API gaps".

## The mockup

`design/mockups/index.html` is one file with invented data, sized like the reference pool (about a million files, 3.8 TiB on disk, 13 datasets). It is written without any library, so it shows the design and not the proposed stack.

| Screen | What works |
|---|---|
| Overview | Capacity bar, shares with coverage, the review and scan cards |
| Space | The list, the treemap and the icicle (switchable), colour by type or age, on-disk or apparent size, the inspector with previews, keyboard navigation |
| Find | All filters, sorting, debounced search over names and annotation text, saved views, "Review these" |
| Review | The whole workflow: fields, every key in the keyboard map except `?` and Alt+Delete, the queue builder with its live preview, background saves, a failed save, an item that has left the disk, describing a folder instead, skipping the rest of a group, the end of a group and of the queue |
| Annotations, Storage | Coverage, the prefix list (add and remove), datasets. Storage has a switch that turns sample snapshots on, so the snapshot presentation can be seen |
| Quarantine, First run | Still pictures of later screens and of states that cannot be reached by clicking |

Two controls in the top bar change the whole mockup: **Scan now** opens the intensity dialog and then runs a sped-up scan, and **Try a state** switches to a stale index or read-only mode. Settings is described in this document and is not in the mockup. Previews are stand-ins, because the mockup makes no requests.

## Users and tasks

**The design optimises for review first and for finding space second.** Review is where the hours go, and finding space is why the tool gets opened. Lookups and housekeeping must be easy but do not shape the layout.

The user is one person: the owner of the server, technically capable, on a desktop browser on the LAN.

| Task | How often | How long | What matters |
|---|---|---|---|
| A. Fill in what things are for (review) | Many sessions | 20 to 60 minutes each | Seconds and keystrokes per item, no lost typing, easy to stop and resume |
| B. Find where space went, and what can go | When the pool fills or on a whim | Minutes | Few clicks from pool to culprit, exact numbers, readable names |
| C. Look something up ("what is this folder?") | Occasional | Seconds | Search, and the annotation visible wherever the item appears |
| D. Housekeeping: scans, prefixes, coverage, quarantine | Occasional | Minutes | Clear state, safe actions |

Measured facts the design relies on (the owner's pool, October 2026):

- 992,440 files in 127,960 folders, 4.2 TiB apparent and 3.8 TiB on disk, 13 datasets, 19,774 hardlinked files, about 17 TiB free.
- A full scan takes 1 to 4 minutes at about 5,000 entries per second when the cache is cold. A low-impact scan could take an hour or more.
- Folder listing 0.1 ms, 100 largest files 3 ms, review queue step 1 ms after a 105 ms first call, name search 130 to 145 ms.

SCOPE.md describes a larger reference deployment (72 TB, 24 TB used). The design does not depend on which is right. It is sized for the stress cases the backend passed: one folder with 1,030,000 files and review groups of 66,000 items.

## Screens and navigation

There are eight destinations. Four carry the product, four are small.

| Screen | Purpose | Build step |
|---|---|---|
| **Overview** | Capacity bar, shares with size and coverage, "continue review", last scan, counts for the finders | 3 |
| **Space** | The breakdown: list, map, inspector | 3 |
| **Find** | Discovery: filters, sort, search, saved views | 3 |
| **Review** | Queue builder and the one-item-at-a-time workflow | 3 |
| **Annotations** | Coverage per share, the prefix list, annotations whose item went missing | 3 |
| **Quarantine** | Items held, purge dates, restore, the undo log | 4 |
| **Storage** | Datasets, compression, snapshots, which clocks each dataset records | 3 |
| **Settings** | Scan schedule and intensity, exclusions, account | 3 |

How the user moves between them:

- The left rail is always visible on desktop. The top bar holds the index status ("Index: today 03:04"), the **Scan now** button and the search box.
- Every item, wherever it appears, opens the same inspector on the right: preview, facts, annotation, actions.
- The three main screens hand off to each other with the current scope kept:
  - Space to Find: "Find inside this folder".
  - Space or Find to Review: "Review what is inside" and "Review these results". The discovery filter and the queue filter are the same structure in the API, so the hand-off is exact, apart from annotation state and prefix, which a queue ignores.
  - Find or Review to Space: "Show in Space" opens the parent folder with the item selected.
- URLs carry the screen and its state (folder path, filters, queue definition), so Back, Forward, reload and bookmarks work. URLs use paths, not entry IDs, because IDs change with every scan.

## Space breakdown

**Verdict: a sorted bar list, always present, with a two-level treemap of the current folder above it. The two are linked: selecting in one selects in the other.**

### What the breakdown has to do

1. Rank the children of the current folder and give exact sizes.
2. Let the user walk down to the culprit.
3. Reveal large things that sit deeper than one level.
4. Keep names readable, because the name is how the user recognises a thing.
5. Let the user act on what they find.
6. Overlay a second fact (type or age) on size.

### Candidates

| Form | Ranks and compares exactly | Shows depth | Names readable | Million children | A few huge files | Verdict |
|---|---|---|---|---|---|---|
| **Sorted bar list** (table with a proportional bar per row) | Best: length on a common baseline, plus the number | One level at a time | Best: full row of text | Works: virtual scrolling, server-side sort, paging | Works: three rows | **Primary** |
| **Treemap** (squarified, two levels) | Weak: areas with different shapes are hard to compare | Best: area is preserved across levels, so deep mass is visible at once | Only in large cells | Aggregates the tail into one "smaller items" block | Works: three big cells | **Companion** |
| **Icicle** (rows of bars, one row per level) | Good within a level | Good to three or four levels | In wide cells only | Slivers merge into a remainder | Works | Close alternative to the treemap |
| **Sunburst** | Worst: angles and arcs, and outer rings exaggerate | Good | Poor: text on arcs | Slivers | Works | Ruled out |
| **Column browser** (one sorted list per level, side by side) | Good | Shows the path taken | Good | Works | Works | Ruled out on desktop width |

What ruled the others out:

- **Sunburst.** It encodes size as angle, which people judge least accurately. Labels follow arcs and cannot hold a file name. A circle wastes the corners of a wide screen. It looks good in a product tour and reads badly in use.
- **Treemap alone.** It cannot rank siblings exactly or show a name for most cells, and it has no natural place for columns such as age, item count or annotation state. As the only view it fails tasks 1, 4 and 5.
- **Column browser.** It needs four or five columns of width and the design already gives the right-hand column to the inspector. Its one advantage, showing the path taken, is covered by the breadcrumb.
- **Icicle as the companion.** It repeats what the list already does well (length encoding, order within a level) and is weaker than the treemap at the one job the companion exists for, which is showing deep mass in a small space. It stays in the mockup as a switch because the call is close and some people find it easier to read.

### How it works

- **List.** One row per child, largest first. Columns: name (with annotation state, prefixes and link count), bar, share of the current folder, size, items inside, last changed. The bar is the row's share of the current folder, so bars in one list add up to the whole. Sort by size, name, items or date by clicking a header. Click selects, double-click or Enter opens a folder, Backspace or Alt+Up goes to the parent.
- **Map.** A treemap of the current folder's children with their children inside them. Click selects, double-click opens. Hover shows size, path, share, item count and age. Every value in a tooltip is also in the list or the inspector. The map can be switched to an icicle or turned off, and the choice is remembered.
- **Inspector.** Preview, facts, annotation and actions for the selected item.

### The hard data shapes

- **A folder with a million children.** The list is virtual: about 40 rows exist in the page at any time, the server sorts, and pages of 500 are fetched as the user scrolls. The header says "1,030,000 items". The map draws the children that are big enough to see and one block labelled "1,029,940 smaller items". When no child is big enough to see, the map is replaced by a summary of the folder: count, average size, and a breakdown by extension. A filter box searches within the folder on the server.
- **A pool dominated by a few huge files.** Three rows and three cells: both forms are at their best. The small remainder gets a two-pixel bar and its number.
- **Tens of thousands of similar files** (a media library). A treemap of equal rectangles says nothing, and the list says it plainly. This is the main reason the list is primary.

### Apparent and on-disk size

One switch, **On disk | Apparent**, applies to bars, map areas, sort order and totals on every screen. The default is on disk, because that is what removal frees. The inspector always shows both. The Storage screen shows the compression ratio per dataset. On the owner's pool the two differ by 0.4 TiB.

### Snapshot-held space

- Snapshot space belongs to a dataset, not to a folder, and cannot be browsed. It is shown in three places: a hatched segment in the capacity bar, a "Snapshots" column in the shares list, and a hatched row and map block labelled "Held by snapshots" at the top level of a share.
- **No snapshots**: none of these appear. Nothing says "0 B".
- **Snapshot figures unavailable** (no `/dev/zfs` and no list file): the Storage screen says so once, with the two ways to enable them. Other screens show nothing.
- **Large snapshot space:** the hatched row sorts by size like any other row, so it can be the first row of a share.

### Hardlinks

- A hardlinked file shows a "2 links" marker. The inspector lists the other paths.
- Folder totals count each file once, at one of its names. Sub-folder totals always add up to their parent. A file row whose size is counted at its other name draws its bar as an outline, and its tooltip says so.
- Because such a row still shows the file's own size, the rows of one folder can add up to more than the folder's total. A line under the list says so and gives the difference.
- Removing one link of several frees nothing. The inspector and the quarantine dialog say "frees 0 B while 1 other link exists".

### Colour modes

- **Type** (default). Blocks and bar segments are coloured by what the bytes are: video, archives and disk images, images, everything else. A folder's bar is a stacked bar of these four. A folder's map block takes the colour of the class that holds more than half its bytes, or the neutral if none does.
- **Age.** Five steps of one hue: under 6 months, 6 to 12 months, 1 to 2 years, 2 to 5 years, over 5 years. The older the data, the further the colour stands from the background. Age is the newer of modified and created time. Read times are off on every dataset of the owner's pool, and the legend says which clocks are in use.

**Flagged concern: folder age.** SCOPE.md defines a folder's age as the newest value anywhere beneath it. That is the right rule for "is this folder still in use". It is the wrong rule for colour, because one new file makes 500 GiB of old data look fresh. The proposal colours folders by where their bytes are: bars are stacked by age bucket, and map blocks take the bucket that holds the most bytes. This needs a per-folder rollup of bytes by age bucket from the backend. Without it the design falls back to the scope's rule.

Both colour modes need per-folder rollups that the API does not have yet (see "API gaps"). Until then folders are neutral and only files are coloured.

## Discovery

**Verdict: one filterable table, with saved views for the common questions and for every later finder.**

- **Filters,** in one row above the table: kind (files, folders), share, type, minimum size, "not changed for", annotation state, prefix, and a search box. The scope ("inside media/tv") shows as a removable chip.
- **Search** matches names and annotation text. A name search across the whole index takes 130 to 145 ms, so the box waits 300 ms after the last keystroke, cancels the request in flight, and runs at once on Enter. Other filters apply immediately because they are fast.
- **Table.** Name with its folder underneath, bar, size, type, last changed, annotation. Click a header to sort. The summary line gives the count and total size of everything that matches, not just the rows loaded. Rows load in pages as the user scrolls.
- **Saved views** are presets of filters and sort: Largest files, Largest folders, Old and large, Not annotated yet. The later finders arrive as more views in the same place: Stale, Duplicates (rows grouped into sets), Orphans, Poorly named, Grew since last scan.
- **Connections.** A row opens the inspector. "Show in Space" jumps to the item's folder. "Review these results" starts a review with the current filter as the queue.

## Review workflow

**Verdict: a three-column screen that never makes the user wait or leave the keyboard.**

### Layout

| Left (236 px) | Middle (320 to 430 px) | Right (the rest) |
|---|---|---|
| The current group and how many items remain in it. The list of groups with a small meter each. The items just done, the current item, and the next few. | The item's name, path and facts. Then the fields: what is it for, prefixes, display name, owner, review after. Then Save and next, Skip, Back. | The preview. |

- The note field has focus when an item loads.
- The item's path is a row of links. Clicking a folder in it is the same as "describe the folder instead".
- Below 1440 px the left column moves above the other two, so the preview keeps its width on a laptop. Below 780 px everything stacks: preview, then fields, then queue. See "Tablet and phone".

### Keyboard map

| Key | Action | Collision check |
|---|---|---|
| Tab, Shift+Tab | Next and previous field. Order: note, prefixes, display name, owner, review after, the buttons, the preview. | Native behaviour, unchanged. |
| **Ctrl+Enter** (also Cmd+Enter on macOS) | Save and go to the next item. With every field empty, it records a skip. | Not used by Chrome, Firefox, Edge or Safari inside a page. Not used by Windows, GNOME, KDE or macOS. |
| **PageDown** | Skip. The skip is recorded. If something was typed, the first press warns and the second press skips. Held-down repeats are ignored. | In a text area PageDown moves the caret by a screen, which this screen does not need. The browser's own Ctrl+PageDown (next tab) is left alone. Keyboards without the key send it with Fn+Down. |
| **PageUp** | Go back to the previous item of this session. Press again to go further back. | As PageDown. |
| **Alt+Up** | Describe the containing folder instead. Press again to climb further. | "Up one level" in Windows Explorer, GNOME Files and Dolphin. Unused by Chrome and Firefox in a page. On macOS, Option+Up moves the caret to the start of the paragraph; the review screen overrides it. |
| Alt+Down | Step back down towards the original item. | The usual "open the list" key for a combo box. The prefix list opens on focus instead, so nothing is lost. |
| Up, in an empty note | Reuse the last saved note, prefixes, owner and review date. Press again for older ones, Down for newer. | No native action in an empty field. |
| F2 | Jump to the display name field. | The rename key in file managers. Unused by browsers. Needs Fn on Mac laptops. |
| Enter or comma, in prefixes | Add the highlighted prefix. Backspace in the empty field removes the last one. | Local to the field. |
| Esc | Close the prefix list, cancel a skip warning, close a dialog. It never skips and never saves. | Standard. |
| ? (outside a field) | Show this table. | None. |
| Alt+Delete (with quarantine, step 4) | Quarantine the item, after a confirmation. | Ctrl+Shift+Delete is the browser's "clear history" and Shift+Delete is "cut", so neither is used. |

The mockup implements every row of this table except `?` and Alt+Delete.

Keys deliberately left alone, and why:

- **Alt+letter.** Firefox on Windows and Linux uses these for its menus (Alt+S opens History). macOS types special characters with them.
- **Ctrl+. and Ctrl+;** open the emoji picker in GTK and IBus on Linux. **Ctrl+Space** switches input method on several systems.
- **Ctrl+S, D, E, K, L, J, U, [ and ]** have browser meanings (save page, bookmark, address bar, downloads, view source, back and forward in Firefox). A page can intercept most of them, and a failed intercept navigates away or opens browser UI.
- **Ctrl+1 to 9 and Alt+1 to 9** switch tabs. **Ctrl+W, T and N** cannot be intercepted at all.
- **F1, F3, F5, F6, F7, F10, F11, F12** belong to the browser.
- **Esc as skip** was considered and rejected. Esc means "cancel, nothing happens" everywhere else, people press it by reflex to close a list, and a skip is a recorded action.

Why PageDown for skip: it is one key with no modifier, it pairs with PageUp for "back", it is free in every browser, and the meaning "next" is obvious. The alternative with the fewest problems is Ctrl+Shift+Enter, which works on every keyboard and is three keys. This is a question for the owner (Q1).

Focus rules that keep the keys working:

- The preview is one Tab stop at the end of the order. While it has focus, the same review keys still work.
- A PDF opens in the browser's own viewer inside a frame. When that frame has focus the page receives no keys at all. The frame is left out of the Tab order, and focus returns to the form when the pointer leaves the preview.
- Shortcuts are ignored while a dialog or the queue builder is open.

### The fields

- **What is it for?** A text area. Enter makes a new line.
- **Prefixes.** Type to filter the list, Enter or comma adds, Backspace removes the last. The list shows each prefix with its meaning and opens on focus, so the user does not need to remember the names. A typed prefix that is not in the list stops the save with a message.
- **Display name.** Optional. The hint says the name on disk does not change. When the naming feature exists, a suggestion appears under the field and one key accepts it.
- **Owner.** Free text with earlier values offered.
- **Review after.** A plain text field that accepts `2027-01-31` or a span such as `6m` or `1y`, and shows the date it resolved to. A native date input would add three Tab stops in Chrome and Firefox.

The fields are checked in the browser against the API's limits before the screen advances, so a rejected save is rare.

### Building the review order

- The current order shows as a sentence at the top of the screen: "Files in all shares, by Share then File type, Largest first". **Change order** opens the builder.
- The builder has four parts:
  1. **What:** files or folders, which shares, a minimum size.
  2. **Group by:** an ordered row of keys (share, file type, extension, folder). Add, remove and reorder them.
  3. **Inside each group:** largest first, oldest first, newest first, or name.
  4. A switch: "Leave out items inside folders that are already described".
- A live preview beside the builder lists the first groups with their counts and sizes: "This gives 978 items in 31 groups. First: media, video: 149 items, 1.2 TiB". It updates on every change. A queue definition is abstract, and the preview makes it concrete. The first queue call takes about 105 ms and later ones 1 ms, so this is cheap.
- Presets: "Largest files by type, share by share" (the owner's order, and the default), "Folders first, largest first", "Biggest files anywhere", "Oldest first, share by share", "One folder at a time".
- The API serves groups largest first by total size. The builder says so. See Q10 for age and prefix as keys.

### Progress and the current group

- The left column leads with the current group's name and one large number: items left in it. Under that: "of 149", a meter, and the whole queue ("978 left in 31 groups").
- The group list shows finished groups as a count, the current group in bold, and the groups to come with the number of items left in each.
- A short list, "Done, now, next", shows the last three items with their outcome (saved, skipped, not saved), the current item, and the next five.
- Progress is stored in `.reflection/` by the backend, so closing the tab loses nothing. The last queue definition is remembered and Overview offers "Continue review".

### Saving

- Ctrl+Enter checks the fields, shows the next item at once, and sends the write in the background. The backend already finishes a write after the browser has moved on.
- A save rewrites the share's whole annotation file and syncs it to disk. On idle disks that can take seconds. This is why the screen never waits for it.
- Writes go out one at a time, in order. A small "saving" mark sits on the item in the "just done" list until the server confirms.

### Edge cases

| Case | What the user sees |
|---|---|
| **No preview available** (archive, disk image, RAW photo, a video the browser cannot decode) | The pane is never empty. It says in one line why there is no preview, shows the facts, and lists the folder the item sits in with the item highlighted. For media, the surrounding folder usually identifies the file. |
| **Very large file** | Nothing is downloaded up front. Video and audio stream by range request. Text shows the first 64 KiB with "Showing the first 64 KiB of 2.1 GiB" and a "more" button. Images over 50 MiB and PDFs over 150 MiB wait for a click. |
| **A folder as the item** | The preview is the folder: totals, and its largest children with bars. Clicking a child previews it in place, with "Back to folder". |
| **Describing the folder instead** (Alt+Up) | The form changes to the folder and a line says "Describing the folder instead of X". On save, the items inside it leave the queue and a line says how many. |
| **End of a group** | The next group starts without an extra keystroke. A line above the form says "Finished media, video: 41 described, 108 skipped. Now: media, image, 23 items". It stays until the next action. |
| **End of the queue** | A summary replaces the form: totals, coverage per share, and the next steps (review skipped items, build another queue, back to Overview). |
| **Save failure** (network, 500) | The user is not interrupted mid-item. A bar above the form says "Not saved: X. Your text is kept", with Retry, Edit and Discard. The text is kept in the browser until the server confirms it. After three failures in a row the screen stops advancing and says so. |
| **Item no longer on disk** (409) | The preview says the item is gone and when it was last seen. A save or skip of it reports "not saved: it is no longer where the last scan found it", keeps the typed note so it can be copied, sets the item aside for the session, and offers a rescan. |
| **Read-only mode** (403) | Known in advance from the session. The fields are disabled and a line says "Read-only mode. You can look through the queue; nothing is recorded." Ctrl+Enter and PageDown just move on. An unexpected 403 switches the screen to this state and keeps the typed text. |
| **Session expired** (401) | A sign-in dialog opens over the screen. The typed text and pending saves are kept and sent after sign-in. |
| **A scan finishes during review** | Entry IDs change. The screen fetches the queue again, finds the current item by its path and keeps what was typed. This needs the index generation in the API (gap 1). |
| **Skip after typing** | The first PageDown shows "You typed something. PageDown again skips and discards it; Ctrl+Enter saves it." |
| **Going back** | PageUp shows the previous item with what was saved. Ctrl+Enter saves changes. PageDown leaves it as it is and returns to the queue. |

### Flagged concerns about the review decisions

These are decided, and the design follows them. The concerns are listed so the owner can weigh them.

- **File-by-file review does not scale by itself.** In the owner's order the first group of a media share is every video file. Most are identifiable from their name, so most presses will be skips. The design adds "describe the folder instead", "leave out items inside described folders" and "skip the rest of this group" to make the same order practical. The first two need the backend to treat items in a described folder as done (gap 5).
- **Tab between fields** is kept exactly. The review date is a text field so that it is one stop, not three.
- **Preview on the right** holds from 780 px up. Narrower screens have no right-hand side, and the preview goes above the fields.
- **One-keystroke skip** is kept, with one exception: after typing, a second press is required, so that a slip does not discard text.

## Annotations, prefixes and coverage

**An annotation can be written and read wherever its item appears. One screen, Annotations, holds coverage, the prefix list and annotations that lost their item.**

- **Annotate anywhere.** The inspector on every screen has the note field, and Ctrl+Enter saves there too. Review is for volume; the inspector is for the one thing the user happens to be looking at.
- **Visible everywhere.** A described item carries a "described" marker and its prefixes in every list. A skipped item says "skipped". An item inside a described folder says "covered".
- **Display names.** Where an item has a display name, it is shown first in the sans face and the real name follows in the monospace face. Both are always visible. Search matches both.
- **Coverage.** The Annotations screen lists each share with its described and skipped counts, the share of bytes inside described items, and a meter. "Review this share" starts a queue for it. The path of the generated `INDEX.md` is shown so the user can open it over the network share.
- **Prefixes.** A table of name, meaning and how many items carry it, with add, edit and reorder. Removing a prefix from the list leaves it on items that have it, and the screen says so. Renaming needs backend support (gap 17).
- **Missing items.** Annotations whose item was not found at the last scan are listed with the last known path. Actions: keep, attach to another item, delete.

## Naming (step 4)

**Naming needs no screen of its own. It is a saved view in Find and a mode of the review screen.**

- A saved view in Find, "Poorly named", lists the candidates with the reason (random string, hash, UUID, few vowels).
- "Review these" opens the review screen in naming mode: focus starts in the display name field with the suggestion filled in and selected. Ctrl+Enter accepts and advances. PageDown skips.
- The suggestion shows where it came from (EXIF date, ID3 tags, PDF title, parent folder).
- A real rename is a separate button, "Rename on disk", with a confirmation that shows the old and new path. It is recorded in the undo log.

## Quarantine (step 4)

**Quarantine is one action available wherever an item is shown, and one screen that lists what is held and when it will be purged.**

- **Starting it.** "Quarantine" is in the inspector, in the selection bar in Find, and on Alt+Delete in review. A confirmation shows what will move, how much space it frees, and two warnings when they apply: "frees nothing while 1 other link exists" and "this dataset has snapshots; space is freed only as they expire".
- **The screen.** A table of held items: name, original location, size, when it was quarantined, and when it will be purged, with a meter for the retention period. Restore is one click. "Purge now" asks first.
- **In the breakdown.** Quarantined space is its own hatched segment in the capacity bar and its own row in a share.
- **Undo log.** A dated list of every move, rename, restore and purge, with Undo on the entries that can still be undone.

## Storage and snapshots

One table, one row per dataset: used, used by files, used by snapshots (hatched bar), snapshot count, compression ratio, and which clocks the dataset records. It states where the ZFS figures came from (`/dev/zfs`, the host script's file with its age, or nowhere) and shows `zfs_error` when a route failed.

## Scans

**A scan is something the user starts on purpose, picks an intensity for, and can watch. On this pool it takes from 1 to 4 minutes at full speed to an hour or more at the gentlest setting.**

### Starting a scan: the intensity dialog

**Scan now** always opens a dialog with three choices. It never starts a scan directly.

| Choice | What it does | Time on this pool | Effect on the server |
|---|---|---|---|
| **Low impact** | One folder at a time, with pauses | About an hour | Other work is not slowed. Disks are active the whole time. |
| **Balanced** | A few folders at a time | About 10 to 15 minutes | Disks are busy. The server stays responsive. |
| **Aggressive** | As many folders at once as the server allows | 1 to 4 minutes | Every disk works flat out. Other disk work slows until it finishes. |

- **Wording.** The dialog says plainly that every choice uses every disk, because file information is spread across all disks of a raidz pool. The choice is how hard the disks work and for how long. It must not suggest that "low impact" leaves disks idle. See Q9.
- **Times** come from the server's own history: "Last scan: aggressive, 3 min 12 s, 1,120,400 items". Until a mode has been run once, its time is an estimate and is marked "about". In the table above only the aggressive time is measured. The other two are placeholders until the backend has the modes.
- **Preselected:** Balanced the first time. After that, the mode used last time. The last choice is stored on the server, so it follows the user between browsers.
- **Keys.** The dialog opens with the preselected choice focused. Arrow keys change it, Enter starts, Esc cancels.
- The dialog reminds the user that browsing continues on the last index until the scan finishes.

### Scheduled scans

Nobody is there to answer a dialog, so scheduled scans use a mode set in Settings. **The proposed default is low impact**, at a fixed time of day. Nobody waits for a scheduled scan, and it should not compete with whatever else runs at night. Settings shows the time the last scheduled scan took in that mode. See Q9 for the one case where aggressive is the better default.

### While a scan runs

- The top bar's index status becomes "Scanning 41%, low impact". A strip under the top bar shows the progress bar, the mode, items scanned of the total, the rate, and the time left.
- The percentage is real. The scan's count matched ZFS's own object count to within 0.1%. When the filesystem gives no count, the strip shows items scanned and elapsed time with no bar.
- "Stop scan" is on the strip. "Finish faster" switches a running scan to aggressive. Both need backend support (gap 7).
- When the scan finishes, the strip says "Scan finished. Showing the new index." and lists what changed in one line. Lists reload in place.

### After a scan

The top bar says "Index: today 03:04". Overview's scan card shows when, the mode, how long, how many items, how many errors, any warnings from annotation reconciliation, and when the next scheduled scan runs.

## First run

**First run is five steps, and each screen says where to find what it asks for.**

1. **Certificate.** The browser warns about the self-signed certificate before any page of ours loads. The setup page repeats the instruction: compare the SHA-256 fingerprint in the container log with the one the browser shows.
2. **Setup** (`setup_required` is true). One page: setup code, username, password twice. The page says where the code is: "Open the container's log in Unraid. The code is printed at start-up." A wrong code names the problem. Repeated failures show the lockout with a countdown from `Retry-After`.
3. **Sign in.** Username and password. A lockout shows "Too many attempts. Try again in 28 s." with a live countdown.
4. **No index yet.** The app opens on Overview with one panel: "Nothing has been scanned yet", the folders that will be scanned (from `RP_ROOTS`), and **Start the first scan**, which opens the intensity dialog. While it runs, the panel shows progress. Every other screen shows the same panel.
5. **First index.** Overview fills in. A one-time hint points at "Add your prefixes" and "Start a review".

## States

**Every state has a visible, worded presentation, and no state leaves a screen blank.** Global states show as a strip under the top bar and as a dot on the index status.

| State | How it is detected | What the user sees |
|---|---|---|
| **No index yet** | `GET /api/scan` has no `index`; data routes return 409 | The first-run panel on every screen. Nothing else is shown half-built. |
| **Scan running** | `running: true` | The strip under the top bar. Browsing continues on the last index. |
| **Stale index** | The index is older than twice the scan interval, or `last_error` is set | An amber dot on the index status and a strip: "The index is 3 days old. The last scan failed: reason." with Scan now. |
| **Scan warnings** | `warnings` is not empty | A line on Overview's scan card that opens the list. |
| **Read-only** | `read_only: true` in the session | A strip: "Read-only mode. Browsing works; annotations, prefixes and quarantine are off." Fields are disabled and the reason is next to them. Action buttons stay visible and disabled. |
| **Request failed** | Network error or 500 | Lists keep their last contents, dimmed, with "Could not load. Retry." in place. No blank screens and no skeletons. |
| **Signed out** | 401 | The sign-in dialog over the current screen, then the request is repeated. |
| **Item gone** | 409 from the content or annotation route | Described under "Edge cases". |
| **Empty results** | No rows | One sentence that names the filters in force and offers to clear them. |
| **Partial scan** | A folder has the error flag | A "could not be read completely" marker on the row and its ancestors. |
| **Not scanned** | A folder has the excluded flag | A "not scanned" marker. Its size shows as unknown, not zero. |

## Visual language

**Principle: the chrome is quiet and nearly monochrome, so that anything in colour is data or state.**

### Type

- Two system font stacks. No font files, so nothing to load and nothing to license.
- **The sans face** carries the interface and everything a person wrote: notes, display names, prefix meanings.
- **The monospace face** carries everything the filesystem wrote: real names and paths. Random names, hashes and UUIDs are easier to tell apart in it. The split also shows at a glance which name is the real one.
- Sizes: 11.5 px for markers, 12.5 px for secondary text, 13 px in tables, 14 px for body and fields, 16 px for an item's name, 20 px for screen titles, 30 px for the one headline figure on Overview. Numbers in columns use tabular figures.

### Spacing and density

- A 4 px step: 4, 8, 12, 16, 24.
- Table rows are 30 px, and 40 px on touch screens. About 25 rows are visible without scrolling at 1080 px height.
- Panels have a one-pixel border and a 6 px radius. There are no shadows except on things that float (menus, dialogs, tooltips).

### Colour roles

| Role | Use |
|---|---|
| Page, panel, sunken | Three cool neutral surfaces |
| Ink, secondary ink, muted ink | Text. All three pass 4.5:1 on the panel surface in both themes. |
| Accent (a deep teal; a light cyan in the dark theme) | Focus rings, the primary button, the selected row, the current nav item. Nowhere else. |
| Type classes | Video (blue), archives and disk images (orange), images (green), everything else (neutral) |
| Age scale | Five steps of blue |
| Hatching | Space that is not live files: snapshots at 45 degrees, quarantine at 135 degrees |
| Status | Good, warning, critical. Always with an icon and a word, never colour alone. |

- **Why three type colours and not nine.** In a treemap any two colours can end up side by side. The palette was checked with a colour-vision simulation for every pair, starting from an eight-hue reference palette. Three hues plus a neutral pass in both themes. No five of the eight do, and the only sets of four that pass do so with a warning in the dark theme. The nine file types remain as text in the type column and as filters.
- **Why hatching.** The hue budget is spent. Snapshot and quarantine space are also a different kind of thing from files, and a pattern says so without a colour that could be mistaken for a type.
- **Age scale.** One hue, five steps, each step clearly lighter or darker than the next, checked in both themes. In the light theme older is darker. In the dark theme older is lighter. In both, older stands out more from the background. Labels inside coloured blocks switch between dark and white text to keep contrast.

### Themes

Light and dark are both designed, from the same tokens, and follow the system setting with a manual override in Settings. The dark theme uses its own steps of each data colour, chosen for the dark surface.

### Accessibility

- Everything works from the keyboard. Focus is always visible as a 2 px ring.
- The list is the accessible form of the map: a real table with the same data. The map is marked as a graphic and takes no Tab stops.
- No information is carried by colour alone. Type and age appear as text in the list and the inspector, and state markers are words.
- Status messages (saved, not saved, scan finished) are announced through live regions.
- Motion is limited to progress bars. `prefers-reduced-motion` turns it off.
- Targets are at least 24 px, and 40 px on touch screens.

### Tablet and phone

- **Tablet in landscape, or any screen from 1000 px:** the full layout. With a keyboard attached, review works as on desktop.
- **780 to 1100 px:** the inspector moves below the list, and below 1000 px the rail becomes a row of tabs. Review keeps fields on the left and preview on the right.
- **Phone:** one column. Space keeps a shorter map above the list. Tables drop their secondary columns. The inspector follows the list. Review stacks preview, fields and buttons, with large Save and Skip buttons, and is usable for a few items. It is not tuned for long sessions, and PDF previews become a link because phone browsers do not show them inline.

## Frontend stack

**Recommendation: Preact, htm and @preact/signals, vendored into the repository as a few small files, with hand-written CSS and ES modules. No build step, no Node, no npm.**

### What that means

- `web/` holds `index.html`, one stylesheet, the application as ES modules, and `web/vendor/` with the library files (a few tens of kilobytes in total) and a note of their versions and checksums.
- The Go binary embeds `web/` with `go:embed` and serves it. The Dockerfile does not change.
- A `-dev` flag serves the files from disk, so editing is save and reload.
- Components are functions that return htm templates. State lives in signals. The treemap (about 60 lines), the bars and the virtual list (about 100 lines) are written by hand. There is no charting library.
- Types are JSDoc comments. Running `tsc --checkJs` in CI is optional. If it ever stops working, the product still builds.

### How it meets the constraints

- **Offline.** Every file is served from the binary. System fonts only. Icons are inline SVG.
- **CSP.** The shell page is served with `default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; media-src 'self'; frame-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`.
  - There are no inline scripts, and htm does not use `eval`.
  - There are no inline style attributes in markup. Bar widths and map positions are set through the style object from script, which a strict `style-src` allows.
  - Images, video and audio use the content route as their source. Text previews are fetched with a Range header and inserted as text, never as markup. PDFs load in a frame of the same origin.
- **Session and CSRF.** One small fetch wrapper adds `X-CSRF-Token` to every request that is not a GET, turns a 401 into the sign-in dialog, and reads the index generation from every response.
- **Five years.** Nothing has to be downloaded to build. The vendored files are plain JavaScript that browsers will keep running.
- **One maintainer with AI help.** Preact components and htm templates are mainstream patterns. The whole dependency surface can be read in an afternoon.

### Alternatives and what ruled them out

| Option | For | Against |
|---|---|---|
| **TypeScript and JSX, bundled by esbuild called from Go** | Typed templates, one output file, still no Node (esbuild is a Go module pinned in `go.sum`) | A build step and one more module. This is the upgrade path if untyped templates start to cost time. The move is mechanical. |
| **Svelte 5 with Vite** | The most pleasant to write, small output, scoped CSS | A Node build stage and a large tree of npm packages. Svelte's component syntax changed substantially between versions 4 and 5. Both work against "builds in five years". |
| **React with Vite** | The most familiar | Everything said about Svelte, with a larger runtime. |
| **htmx with Go templates** | No client framework, fits Go | The JSON API already exists and is tested, and this would add a second, HTML-shaped API beside it. The review loop (advance before the save returns, keep an outbox, prefetch the next preview) and the treemap are client-side state by nature. htmx also injects inline styles unless configured not to. |
| **Alpine.js** | Tiny | Its normal build evaluates expressions from attributes and needs `unsafe-eval`. The CSP build is too limited for this application. |
| **Lit** | Web standards, no build needed | Shadow DOM makes shared theming and a single Tab order across components harder. It gains nothing here over Preact. |
| **No library at all** | Zero dependencies | Workable, and the mockup is written this way. The review screen and the virtual lists would need a hand-made rendering layer, and Preact is that layer in a few kilobytes. |

### To verify in the first week of step 3

- Preact's style handling under the enforced CSP in Chrome and Firefox.
- The browser PDF viewer inside a same-origin frame under this CSP in Chrome and Firefox.
- Which video files actually play. Chrome plays many MKV files and Firefox historically did not; HEVC depends on the machine.

## API gaps

**Eight gaps block the first release, and eleven more are needed for the recommended presentation.** The gaps are numbered for reference. "Blocks" means the first release is wrong or unsafe without it. "Needed" means the recommended presentation depends on it and a fallback exists.

### Blocks the first release

1. **Index generation.** Entry IDs come from a counter in a parallel walk and are reassigned by every scan. After a scan, an ID held by the browser can name a different item.
   - Every response carries the index generation (a header such as `X-RP-Index`, and `index.id` in `GET /api/scan`).
   - `PUT` and `DELETE /api/entries/{id}/annotation` and `GET /api/entries/{id}/content` accept the generation the client believes in and answer 409 with code `index_changed` when it differs.
   - `GET /api/entries/lookup?path=` returns the entry at a path (the index already has `Lookup`). The UI uses it to find its place after a scan and to put paths, not IDs, in URLs.
2. **Totals for `/api/entries`.** The response has `items` only. Add `total`, `total_size` and `total_disk` for the filter.
3. **Subtree filter.** `under=<entry id>` in `Filter`, for `/api/entries` and `/api/queue`. This is what links Space to Find and Review.
4. **Error codes.** Errors are `{"error": "message"}`. 409 means four different things (item gone, no index, scan running, setup done). Add a stable `code` field.
5. **Review queue.**
   - a. `exclude_covered`: treat items inside an annotated folder as done. Coverage today ignores a note on the share itself; the queue should follow the same rule, or one note on a share would empty its queue.
   - b. Totals: the number of groups including finished ones, and total items and bytes, so the screen can say "group 3 of 31".
   - c. A gone item cannot be skipped, because the skip reads the item from disk first and returns 409. It stays at the head of the queue until the next scan. Allow `{"skipped": true}` without the disk read, or accept an `exclude` list of IDs.
   - d. An `offset` or cursor, so that the queue can be browsed in read-only mode, where nothing ever leaves it.
   - e. A bulk write, `POST /api/annotations/bulk`, by ID list or by filter, for "skip the rest of this group". One group can hold 66,000 items and each single write rewrites the share's whole file.
6. **Empty annotations.** A `PUT` with every field empty and `skipped: false` stores a record that removes the item from the queue and counts as "annotated" in the state filter, yet is not counted in share coverage. Reject it or store it as a skip. The UI will send a skip in this case either way.
7. **Scans.**
   - `POST /api/scan` takes `{"intensity": "low" | "balanced" | "aggressive"}`. Today every scan is what "aggressive" will be.
   - `GET /api/scan` reports `intensity`, and `index` records the intensity and duration of the scan that built it, per mode, so the dialog can show real times.
   - A default intensity for scheduled scans (`RP_SCAN_INTENSITY`, and a setting), and the last manual choice stored on the server.
   - `next_scheduled` and the interval in the status. A time of day for the schedule, not just an interval from start-up.
   - `DELETE /api/scan` to stop a running scan. Optionally, a way to raise the intensity of a running scan.
8. **Serving the UI.** `GET /` and the static files from the embedded folder; the CSP above for the shell page (today every response carries `default-src 'none'` and `X-Frame-Options: DENY`); `index.html` for client-side routes; cache headers for static files (today everything is `no-store`).

### Needed for the recommended presentation

9. **Per-folder rollups by type and by age.** Bytes per category and bytes per age bucket for each directory, on `Row` or from `GET /api/summary?id=`. Used by the stacked bars, the map colours, the capacity bar and the million-children summary. Fallback: folders are neutral and age uses `max_mtime`.
10. **`/api/tree`.**
    - `depth=2` with a minimum size, to draw the map from one request. Fallback: one request per large child, which is acceptable at 0.1 ms each.
    - `name=` to filter inside one folder.
    - Keyset paging (`after=`) for very large folders in place of a growing `OFFSET`.
    - No change is needed for the "smaller items" block: the browser can compute it as the folder's total minus the rows it has loaded, counting a file row only when `counted` is true.
11. **Search and filters.**
    - Search in annotation text (note, display name, owner). SCOPE.md feature 2 asks for it; the API matches names only.
    - `modified_after`, a created-time range, `min_disk` and `max_disk`, "has more than one link", and the excluded and error flags.
    - Sort by created time. Age in this product is the newer of modified and created, and the sort keys have modified only.
12. **`GET /api/entries/{id}`.** Add the chain of ancestors (ID and name) for breadcrumbs, `covered_by` (the nearest annotated ancestor), and the target of a symlink.
13. **`GET /api/storage`.** Add pool totals (capacity, used, free), and for each dataset the ID of the share entry it corresponds to, so that snapshot figures can be attached to rows.
14. **Preferences.** `GET` and `PUT /api/prefs` for a small JSON document: size mode, units, the last queue definition, saved queues, the map form. Fallback: browser storage, which does not follow the user to another browser.
15. **Queue keys.** The owner listed age and prefix as order keys. The API groups by share, type, extension and folder, and orders by size, modified and name.
    - Add an age-bucket group key and ordering by created time.
    - Add queues over items that are already annotated or skipped (for example "skipped items" or "has a prefix, no note"). Today the queue clears the state and prefix filters.
16. **Session.** Add the scan roots and the scan interval to `GET /api/session` for the first-run and settings screens.
17. **Prefixes.** A count of items per prefix, and a rename that updates the items that carry it. Replacing the list cannot express a rename.
18. **`GET /api/shares/{id}/annotations`** returns everything at once. Add paging and a filter for missing items.
19. **Previews** (optional, see Q8). Thumbnails for RAW and HEIC photos and poster frames for video the browser cannot decode would need a media tool in the image.

### Step 4, each with its feature

Staleness (which clocks count, per dataset), naming candidates and suggestions, rename on disk, quarantine (move, list, restore, purge, retention), the undo log, the duplicates job (start, progress, sets), the orphan finders, and growth between scans.

## Questions for the owner

**Fifteen decisions, each with a recommended answer. Q1, Q2 and Q9 change the most.**

| # | Decision | Recommended |
|---|---|---|
| Q1 | The skip key: PageDown, Ctrl+Shift+Enter, or Esc. | **PageDown**, with PageUp for back. |
| Q2 | Add the folder-level review tools (describe the folder instead, leave out covered items, skip the rest of a group)? They need gaps 5a and 5e. | **Yes.** File-by-file review of a million files is not practical without them. |
| Q3 | The map beside the list: treemap, icicle, or none. | **Treemap**, switchable to icicle or off. |
| Q4 | Type colours: three classes plus a neutral, or one colour per file type. | **Three plus neutral.** It is the largest set that stays distinguishable for colour-blind users in a treemap. |
| Q5 | Colour by age: by where the bytes are (needs gap 9), or by the newest item beneath. | **By bytes.** Newest-beneath stays as the folder's stated age. |
| Q6 | Units: binary (GiB, TiB, as `zfs list` prints) or decimal (GB, TB, as drives are sold). | **Binary, labelled GiB and TiB**, with a setting. |
| Q7 | Default size: on disk or apparent. | **On disk.** |
| Q8 | Files the browser cannot show (HEVC in MKV, RAW, HEIC): accept the fallback, or add a media tool to the image for thumbnails. | **Accept the fallback** for the first release. Revisit after real use. |
| Q9 | Scan modes. (a) Scheduled scans: low impact, or aggressive. (b) Is the concern that disks are busy, or that they are woken at all? | **(a) Low impact at a fixed time.** (b) If the disks normally spin down, no mode avoids waking them, and a 2-minute aggressive scan keeps them awake for less time than an hour-long gentle one. In that case set scheduled scans to aggressive. |
| Q10 | Age and prefix as review-order keys. Age can order items within a group today. Prefix only applies to items that are already annotated. | **Age: add buckets as a group key. Prefix: support it for second-pass queues** (gap 15). Please confirm what a prefix-ordered review should do. |
| Q11 | Ctrl+Enter with every field empty: record a skip, or refuse. | **Record a skip.** |
| Q12 | At the end of a group: carry on, or pause for a keypress. | **Carry on**, with the line that says what finished. |
| Q13 | The build: no build step, or TypeScript with esbuild from Go. | **No build step.** |
| Q14 | Phone: full support for long review sessions, or the single-column layout described. | **The single-column layout.** |
| Q15 | The scan dialog's first default. | **Balanced**, then the last choice. |

## Build sequence

**Step 3 starts with the backend gaps that block it, then builds Space, Review and Find in that order. Step 4 adds one feature at a time, each into a place the first release already has.**

### Step 3: first usable release

| Order | Work | Depends on |
|---|---|---|
| 3.0 | Backend: gaps 1, 2, 4, 6, 7 and 8 | |
| 3.1 | Shell: embedded files, CSP, routing, setup and sign-in, the fetch wrapper, index status, the scan dialog and progress strip, read-only and error states, the theme tokens | 3.0 |
| 3.2 | Space without the map: the virtual list, breadcrumbs, the inspector with preview and note. The product is useful from here. | 3.1 |
| 3.3 | Review: the form, the keys, background saves with the outbox, progress, the queue builder, prefix management | 3.2, gap 5 |
| 3.4 | Find: filters, table, search, saved views, the hand-offs to Space and Review | 3.2, gaps 3 and 11 |
| 3.5 | The map and colour modes, Overview, Annotations, Storage, Settings | Gaps 9, 10 and 13 |

Review comes third and not first because it reuses the inspector's preview and note components. The map comes last because it depends on the rollups, and the list already answers most space questions.

### Step 4: remaining features, each as backend plus UI

| Feature | Where it lands |
|---|---|
| Staleness | A "Stale" view in Find; the age mode coloured by bytes; clocks per dataset on Storage |
| Naming | The "Poorly named" view; naming mode in Review; "Rename on disk" in the inspector |
| Quarantine | The Quarantine screen and undo log; the action in the inspector, Find and Review; hatched space in the breakdown |
| Orphans | Two views in Find: downloads never imported, appdata without a container |
| Growth | A "since last scan" column in Space; a "Grew" view in Find; a line on Overview |
| Duplicates | A job panel (start, progress, cached results) and a grouped table in Find |
