# Library data: what is kept where, and who writes it

> The rules and scenarios that came out of this note are in
> [library-spec.md](library-spec.md). Where the two differ, the spec is
> correct. This note keeps the reasoning.

A design note, written 2026-11-28 for Dean's question: should the one
SQLite index be split into an inventory, a personal database and the
manager's data? It answers that, says what a host helper (mlhost today,
NFS hosts later) can build, sets out Dean's sync rules (adds come from
anywhere, deletes only in the app, losses put back), lays out his target
setup on it, and lists what is cut on purpose. Nothing here is built.
Dean's decisions are listed at the end. An independent review of the
first draft found real gaps (the setup order relied on two things that
don't exist, cached hashes were trusted where the docs refuse them,
published user data leaked listening history to readers); this version
answers them.

## The short answer

Yes, split it, along one rule rather than three topics: **every file
has one writer at a time**. The library's files (catalog, records,
shared playlists) are written by whichever writer holds the sync lock;
an inventory by the one helper or device that builds it; a personal
database only by its own installation. Then nothing has to be merged
on write.

| Piece | What it holds | Written by | Where |
|---|---|---|---|
| **Catalog** | The library: tracks, file ids, tags, art references, audio hashes, the deletions log | Any writer, holding the sync lock | Published to every replica as today's snapshots |
| **Inventory**, one per place | Facts about the files at one place: path, size, time, content hashes, audio hash, parsed tags and art | Whoever reads that place fastest: its host helper, or a writer for a disk it can read | `.medialayer/host/` on that place |
| **Personal data**, one per installation | Playlists, ratings, play counts made on that installation | That installation, only | On the device; cloud sync later. Shared playlists are published into the library |
| **Records** | Which library, which primary, which copies, each copy's state | Any writer, holding the sync lock | `repo.json` and `member.json` on the replicas, `settings.json` on the device (as now) |

Dean's three parts map onto this with two changes:

1. **"Inventory" is two things.** What is physically at a place (a fact
   anyone with fast access can measure) is not what the library is
   (which tracks, under which ids, which playlists point at them).
   Helpers build the first. Only a writer makes the second, mostly
   from the primary's listing and inventories.
2. **The manager's part needs no new database.** It already lives
   outside the index, in `repo.json`, each replica's `member.json` and
   the device's `settings.json`, and readers already ignore it apart
   from finding the snapshot. It stays as small JSON records.

## What the index holds today

`internal/index/index.go`:

| Table or key | Kind | Notes |
|---|---|---|
| `file`, `tag`, `art`, `file_art` | Catalog | Almost all of it, as Dean says. `file.sha256` is filled only where the cloud lists SHA-256, so never on Drive or Proton |
| `playlist`, `playlist_item` | User data | Each edit marks the index dirty, so the next sync publishes a new version of the whole index to every replica |
| `location` | Device bookkeeping | Each file's cloud id on the primary, under a replica name hard-coded `gdrive` (`indexer.go:32`) |
| `meta`: `library_id`, `generation`, `version`, `version_id`, `version_parent`, `audio_hash_version`, `schema_version` | Catalog | They identify the version |
| `meta`: `dirty`, `files_dirty`, `playlists_dirty`, `primary_version` | Device bookkeeping | The dirty flags are zeroed in each snapshot (`repo.go:383`); `primary_version` is this device's record of what the primary holds |

So the index is already mostly catalog. The change is to move
playlists out (and never put ratings or play counts in), and to move
`primary_version` and `location` into a local file next to it.

## Why the split pays

- **Listening must not republish the library.** Play counts change on
  every track played. In today's design each change is a new index
  version, a snapshot of the whole index (megabytes for 16,800 tracks)
  uploaded to the primary and every replica, and on Proton each small
  write takes 6 to 12 seconds (promote-review.md). Playlists already pay
  this. With user data in its own small file, the catalog changes only
  when the music does.
- **Readers add their own without writing the shared folder.** A
  daughter's playlists, ratings and counts live in her own file on her
  phone; the catalog she reads is never touched (milestone 6 already
  plans this overlay; the owner now uses the same shape).
- **Helpers do the slow part.** Everything that needs every byte of
  every file (tags, art, audio hashes, content hashes) is inventory, and
  can be built where the bytes are.
- **Comparing sources gets cheap.** Two inventories with hashes can be
  compared without reading a file.

## The catalog

The SQLite index as now, without the playlist tables, `location` and
`primary_version`, plus a deletions log (below). Published as
`.medialayer/snapshots/NNNNNN.sqlite` with its version and version id,
as sync.md describes; that machinery is unchanged.

- **Writers take turns** through the sync lock, and the version id
  check makes each start from the newest catalog (see "Writers and
  readers").
- **What changes it:** indexing the primary, the hash pass, a tag edit
  made in the app, an import, a delete made in the app. Not playlists,
  ratings or counts.

## Inventories, and what a helper can build

An inventory describes one place. The host helper already writes one
(`.medialayer/host/inventory.json.gz`: path, size, time in ms, inode and
change time, parsed tags, art by SHA-256, audio hash). For it to serve
compare and adoption, three changes:

1. **Content hashes**: SHA-1 (what Proton lists), MD5 (what Drive lists)
   and SHA-256, computed in the same read as the audio hash, so they
   cost no extra disk time. They live in inventories only, not in the
   catalog. A file whose change time moved loses its content hashes, even
   where it keeps its audio hash today (`hostscan.go:277-283`), since an
   in-place retag that keeps size and time looks exactly like that.
2. **Priming, before a source is attached.** Dean's decision: the
   helper's job is to prime the inventory on its host before the folder
   is added to any library, so adding it is a matter of minutes, not of
   hashing 483 GB over the link. So mlhost runs on any folder and makes
   `.medialayer/host/` itself (today it refuses a folder with no
   `.medialayer`, `hostscan.go`, `ErrNoRepo`), and a folder whose
   `.medialayer` holds only `host/` counts as not yet a library in sync's
   checks. The device uses the inventory of any source or copy, not only
   of the folder it indexes (`host.go`).
3. **A fresh pass on request.** For adoption (below), the device asks
   for a pass that starts after its request and waits for that
   inventory, using the probe mechanism smb.md describes for level 2.

Who builds which inventory:

| Place | Built by | How fast |
|---|---|---|
| Linux server over SMB (and NFS later) | mlhost on the server | Disk speed; an hour or two for 483 GB, then only changed files |
| External SSD | The Mac it is plugged into, reading it directly | Disk speed, while plugged in |
| A folder on the Mac | That Mac | Disk speed |
| Proton, Google Drive | Nobody: no host to run on. Their listings give a content hash per file | Listing only |

NFS needs nothing new in the helper, which reads a folder on its own
disk however devices reach it. On the device side, as far as I know
rclone has no NFS client backend (to be checked against the pinned
v1.76.0); on a Mac, an NFS share mounted by the system is a local
folder to rclone, which is the simple way in.

### What an inventory may decide

smb.md's rule stays: **an inventory supplies facts that make work
cheaper, never decisions that remove or replace a file.**

- Allowed: tags, art and audio hashes for the catalog (level 1, as
  built); the compare step of an import, which only lists and copies in;
  finding a moved file.
- Not allowed: standing in for the real listing when deciding what to
  copy, replace or remove on a replica (level 2, still shelved); and
  proving two files equal from a cached hash, which consolidate.md and
  smb.md both refuse because rclone's `Equal` then sets the time and a
  false match is never looked at again. Adoption uses fresh hashes.
- A cloud's listed hash can be empty, and Proton's SHA-1 is the one the
  uploading client recorded, not one Proton checks. An empty hash means
  read the file; a Proton hash is trusted only for files MediaLayer
  uploaded itself, after the check below.

## Personal data

Playlists, ratings and play counts leave the library's database for a
**personal database**: one SQLite file per installation, kept on the
device and ATTACHed to the catalog so the app can join them (milestone
6's plan). Decided by Dean, 2026-11-28.

- **On the device only.** It isn't published to the library's copies.
  Syncing it to a cloud of the user's choosing (not necessarily the
  library's) is a later milestone, for writers and readers alike.
- **Sharing playlists** is for writers only. A writer can share a
  playlist, which publishes it into the library as
  `.medialayer/playlists/shared.sqlite`, written under the sync lock like
  the catalog and copied to every copy. Readers see shared playlists
  read-only beside their own and can't share theirs, since they can't
  write the library. Ratings, play counts and last-played times are
  never published, so Dean's listening never reaches his daughters'
  copy.
- **Ready to sync from day one.** Playlist ids are stable (ULIDs, as
  now), each playlist and rating has an `updated_at`, and a removed
  playlist leaves a tombstone. Then the later sync can combine one
  person's installations without a format change: play counts add up,
  a rating or a playlist takes its newest version.
- **Naming tracks.** Every reference to a track (a playlist item, a
  rating, a play count, a shared playlist's item) keeps four things:
  the file id, the whole-file hash, the audio hash and the path (Dean,
  2026-11-28). A reference finds its track by, in order:
  1. **the file id**, which survives retags and moves within this
     catalog;
  2. **the exact whole-file hash**, which survives a catalog that was
     rebuilt or a file brought back under a new id;
  3. **the audio hash, when one library track has it**, which survives a
     retag on top of either;
  4. **the audio hash shared by several tracks** (versions, or an album
     and a compilation): the one at the reference's path if there is
     one, otherwise the app asks which;
  5. **the path alone**, only for a reference saved before its track had
     any hash.

  A reference that matches by anything but step 1 or 2 is shown as
  found by its audio, so a wrong guess is visible.
- **Keeping references current.** A retag changes the whole-file hash,
  so stored hashes go stale. Whenever the app opens a new catalog
  version, it refreshes each reference found by file id with that
  file's current hashes and path. A reference the catalog's file id no
  longer knows is looked up by the catalog's previous whole-file hash
  (kept for every file) before the audio hash, so a retag the reference
  missed is followed exactly. Rows are often saved before the hash pass
  reaches a file, so the indexer fills `audio_sha256` from an inventory
  where it can, and the next refresh adds it to the reference.
- **Moving today's playlists.** The first release with this moves the
  playlist tables from the index into the device's personal database.
  For a new library on Proton (below), Dean's current playlists are
  carried by path (`PathPlaylist`, `playlist.go`), then matched by audio
  hash once the new catalog has hashes.

Until that later milestone, a playlist made on one Mac isn't on Dean's
other Mac or his iPhone unless he shares it.

## Writers and readers

A **writer** is any MediaLayer installation, on any device, connected
to the primary with write access: Dean's Mac today, and later a second
Mac or an iPhone set up the same way. A **reader** has no write access (his daughters, or an installation
opened on a view-only share) and writes only its own personal database.
Dean decided this on 2026-11-28, in place of the first version's single
controller.

Several writers are what today's sync already allows, one at a time,
through the sync lock (sync.md). What keeps them consistent:

- **One at a time.** Every write to the library (indexing and
  publishing the catalog, imports, restores, deletes, tag edits, shared
  playlists, the records) happens inside a sync job holding the lock.
- **Always on the newest catalog.** Each sync checks that the primary's
  newest snapshot is the one this writer last made or took (the version
  id check, already built); if another writer published since, it takes
  that catalog and indexes on top of it before doing anything. With
  playlists out of the catalog, there is nothing left of today's
  playlist carrying between devices' indexes.
- **Decisions come from the files and the shared logs, not from a
  device's memory.** The deletions log, the content hashes and the
  versions' "not duplicates" marks are in the catalog, so a delete made
  on one Mac is applied by a sync from the other.
- **Work a writer starts and then sees invalidated stops.** A tag edit
  or delete names the catalog version it was made against; if the
  catalog moved and the file changed meanwhile, it asks again rather
  than overwriting the other writer's change.

What multiple writers add to the work list, which one writer would
have avoided:

- **One member list.** Today each device lists its own secondaries in
  `repo.json`, so two devices overwrite each other's (sync.md, Known
  gaps). `repo.json`'s members become the library's list: a writer adds
  or removes a copy there under the lock, and every writer takes its
  copies from it, asking for any sign-in it lacks.
- **Following a Make Primary.** A writer that finds the primary moved
  (a newer generation in `repo.json`) follows it, instead of stopping
  with "the primary moved" as now.
- **A second Mac joining.** Open the library from any copy or the
  primary: the installation reads `repo.json`, asks for the sign-ins,
  takes the newest catalog and is a writer. This is also what replaces a
  lost Mac, so no separate takeover is needed; today picking a secondary
  is refused and Choose Music Folder browses Google Drive only.
- The offline fix's "this copy was written to" comes from the members in
  `repo.json`, not only from `settings.json`, so every writer knows it.
- Both Macs run automatic syncs; the lock makes the second wait, and
  each check stays cheap when nothing changed.

## Sync rules

Dean set these rules on 2026-11-28, after the first version of this
note. They replace its "held for review" design and its "replicas stay
exact copies, extras removed" rule. They come down to one:

> **Only the primary is the library. Each sync makes every secondary
> match it, path by path: a path the library has gets the library's
> file, and a path it doesn't have is brought into the library, unless
> its bytes are already there.**

A writer's change sticks only when it is made on the primary. Anything
done to a secondary directly, by MediaLayer or anything else, is undone
the next time an instance that has it as a secondary syncs, except a
file at a new path, which joins the library. The five rules, as Dean
put them:

1. **Any file a source has that the library doesn't is copied to the
   primary, then to every other copy.** Every sync, not only when the
   source is added, and both versions are kept.
2. **Any file the library has that a source lacks is copied to it.**
3. **A file deleted from a copy outside MediaLayer is copied back to it
   from the primary.**
4. **A file deleted from the primary outside MediaLayer is copied back
   to the primary** from a copy that still has it and whose content
   matches the catalog.
5. **The only way to delete is in MediaLayer.** The delete is made on
   the primary and pushed to every copy.

So sync is two-way for adds and one-way for everything else: music can
enter from any place, but only a writer edits or removes it, and
nothing a listing shows missing is ever taken as a delete.

### What counts as new (rule 1)

A file on a secondary that isn't the library's file at that path is
classified by its path, then its hashes (from that place's inventory, or a fresh read
for a disk):

| The file on the copy | What happens |
|---|---|
| Content hash already in the library, at any path | Not imported: the library has these bytes. If the copy lacks the file at the library's path, it is moved there instead of copied again; otherwise this redundant copy goes to that copy's trash, the one removal made without a person, since no bytes are lost |
| At a library path, with the library file's content from before its last change | A stale copy of a file changed on the primary (a retag, say): replaced with the primary's, old version to trash. This is how a retag reaches the secondaries |
| At a library path, with any other content (a retag made on the secondary, damage) | Not imported. The library's file is put back, the secondary's version to its trash for 30 days, and the sync reports it |
| At a new path, same audio hash as a library track, other content (a retagged copy saved elsewhere) | Imported as a **second version** of that track, at its own path |
| At a new path, fails to decode (FLAC, ALAC) | Not imported: damaged, and left out of the library. A damaged MP3 or AAC can't be told, and comes in |
| At a new path, anything else | Imported at its own path |

A hash that leads to removing or replacing a file on a copy must be
current: read now on a disk, or on the server an inventory row whose
content hashes were made after the file's present change time (the
helper drops them otherwise). A cached hash may only keep a file from
being imported. Whatever is removed or replaced goes to trash.

Versions are shown together, as one track with a choice "pick a
keeper"; picking deletes the others, in MediaLayer (rule 5). The same
audio on an album and a compilation is a legitimate pair, so versions
are never merged automatically, and a pair can be marked "not
duplicates" to stop the app asking. Before a source's first import, the
app shows the counts (files, size, versions) and asks once; after that,
imports run on their own and the chip says what came in.

Two consequences are accepted on purpose: a retag made directly on the
server is undone (kept 30 days in its trash), since a change sticks only
on the primary; and anything that writes music at new paths in a
secondary's folder (a downloader on the server, say) feeds the library.

The catalog keeps each file's whole-file hash as the primary lists it
(SHA-1 on Proton, MD5 on Drive, or the inventory's on SMB), and its hash
from before its last change. The current hash restores the primary and
recognises moves and duplicates; the previous one recognises a
secondary's stale copy, even where the file was also moved, so it is
moved and replaced rather than copied again.

**Same audio, different tags.** Two files with one audio hash and
different whole-file hashes are two library files when the primary has
both, and the primary's catalog says which is which. A retag of one
replaces only copies holding that file's previous whole-file hash, so
the other version is never touched. The audio hash only groups versions
for "pick a keeper" and for the tag-edit rule.

### Adding and removing on the primary

- **A file that exists on the primary is in the library.** However it
  got there (MediaLayer, Proton's app, a shell on the server when the
  server is the primary), the next sync indexes it and copies it to
  every secondary. Nothing else is needed to add music.
- **A file removed from the primary outside MediaLayer is not removed
  anywhere else.** The next sync finds it on a secondary and copies it
  back to the primary (rule 4, below).
- **The only way to delete is from within MediaLayer**, which removes it
  from the primary and then from every secondary (rule 5, below).

### Restoring the primary (rule 4)

A file gone from the primary and not in the deletions log:

- **Moves first.** A file whose content hash appears at a new path in
  the primary's listing was moved or renamed, not lost: the catalog
  keeps its file id, and each copy moves its own file rather than
  copying again. Without this, a folder reorganised in Proton's app
  would come back at its old path as well.
- **Then restore**, from a copy whose file's content hash equals the
  catalog's for that file, preferring the SSD (a mass restore from the
  server is about 90 hours at 1.5 MB/s), under the old file id, so
  playlists and ratings stay. It is a write of music to the primary, so
  it follows the merge's rules (lease per file, free path checked just
  before, temporary name; consolidate.md).
- **No good copy on any source:** see "Missing everywhere" below.
- **A bad listing is not a mass loss.** A Proton listing that fails,
  ends early, or lists far fewer files than the catalog must stop the
  sync with an error, not start a 483 GB restore. The offline fix covers
  the primary too.

### Missing everywhere

A file gone from the primary and from every secondary (decided by Dean,
2026-11-28):

1. **Trash first.** Before calling it missing, the sync looks in each
   source's trash (the primary's and every secondary's `.medialayer/trash`)
   for a file with the catalog's whole-file hash, and restores it from
   there as from a secondary.
2. **Otherwise the catalog keeps the row, marked `missing`** (the column
   exists), with its file id, so playlists and ratings still point at
   it, and the track shows as missing. Whenever any source later shows
   that exact whole-file hash (an offline drive plugged back in, a file
   put back by hand), it is restored to the primary automatically, under
   the same file id. Until then the user can, in MediaLayer:
   - **relink** it to another file in the library, which takes over its
     file id, playlists and ratings;
   - or **delete** it, which writes the deletions log like any delete.
3. **Another version may already be there.** If the library holds a file
   with the same audio hash (normal sync will have brought in any such
   copy a source had), the missing track says so and offers to delete
   the missing entry in favour of it, moving its playlists and ratings
   across. It always asks first.

A missing entry is never removed on its own, however long it stays
missing.

### Deleting (rule 5)

The writer records each delete (path, file id, size, content and
audio hash, time) in a deletions log in the catalog. It removes the
file from the primary to the primary's `.medialayer/trash`, then from
each copy to its trash, 30 days, as now. A copy's file is removed only
when the log names it, the primary's current listing lacks that path,
and the copy's file is the logged version, so a delete followed by a new
file at the same path never removes the new one from a disk that
returns later. The log is never pruned by age; each copy's
`member.json` records how far into it it has applied. Removals no longer
count toward the daily limit.

### Tag edits

A tag edit made in MediaLayer is written to the primary and reaches
every copy by path, as any change does. Other library tracks with the
same audio hash (versions, or the same recording on a compilation) get
a field only if it had the same value there as on the edited track
before the edit. So a fixed title or artist reaches every version and
the compilation, while album, track number, disc, album artist and
cover, which already differed, stay. When that would change another
track, the app asks: "Also apply the title change to 2 other tracks
with this audio?", yes by default for versions, asked for other albums.

### What stays from today

- The trash on every copy, 30 days.
- The daily limit, only for replacements caused by files changed on the
  primary outside MediaLayer: the guard against a primary whose files
  are being rewritten wholesale (corruption, ransomware). Putting a
  secondary back to match the primary doesn't count: that is the rule
  working, and its trash keeps what it replaced.
- Every copy holds every library file at the library's path, byte for
  byte, once its sync ends, so any copy can be promoted. A file that a
  copy contributed is in the library by then, not an extra.

## Dean's target, step by step

Proton is the primary, the SSD and the server are copies, the Mac is the
writer (a second Mac could be another). Later a Google Drive copy is shared view-only with his
daughters.

### Getting there

The app builds in no order. **The first source added becomes the
primary** (Dean's decision), and everything else follows from the sync
rules, so this is just Dean's plan of steps. It needs no Make Primary
of 16,800 tracks and none of consolidate.md's "filling a copy from
another copy" (section 6). It does need two pieces that don't exist:
**any kind of source as the first one**, Proton included (Choose Music
Folder browses Google Drive only, and a Proton primary is reached today
only by Make Primary), and **mlhost priming a folder that isn't yet a
library** (above).

1. **The offline fix** (consolidate.md, section 5): a replica once
   written to that is missing is offline, never empty.
2. **Measure an upload to Proton** of a few hundred tracks from the Mac:
   Proton's per-file overhead (about 14 requests per small file) and its
   known idle upload line make the home upload speed only an upper
   bound. At 100 Mbit/s, 483 GB is about 11 hours; at 20, two days.
3. **Choose which source goes first.** For a track at the same path on
   both with other tags, the first source's file is the library's, and
   the other's is replaced by it when that source becomes a secondary
   (kept 30 days in its trash). Only files at paths the library lacks
   come in, as files or versions. So import the better-tagged one
   first; the count shown before the server's first sync says how many
   of its files would be replaced.
4. **Inventory both**: mlhost with content hashes on the server, the
   Mac reading the SSD.
5. **Add an empty Proton folder first**, so it is the primary.
6. **Import the SSD into it.** Every file is "only there", so this is
   the whole upload, resumable under the lease across days. The import
   reads each file on the Mac anyway, so it records each file's parsed
   header as it uploads, and indexing Proton uses those records (by
   path, size and the SHA-1 the upload produced) instead of fetching a
   block per track from Proton. The merge's rules for free paths and
   temporary names (consolidate.md) need testing on Proton first, which
   is case-blind and leaves drafts.
7. **Check Proton.** Download and hash a sample of the uploaded files
   (a few hundred, across the library), and in the background, over the
   following days, all of them once. Until that passes, the SSD and the
   server take no replacements.
8. **Adopt the SSD as a copy.** Every file is checked by a fresh read on
   the SSD against Proton's SHA-1 of the file MediaLayer uploaded; they
   match, so nothing is copied.
9. **Add the server as a copy**, after a fresh mlhost pass requested
   for it. Files equal by fresh hash are kept where they are. Rule 1
   imports what is at paths the library lacks: only those
   cross the 1.5 MB/s link (about 5.4 GB an hour). Rule 2 fills the
   server with the rest, uploaded into the server's site, its fast
   direction.

### Keeping three copies in step

"Always in sync" means: after each change, a sync starts on its own and
reaches every replica that is online; one that is offline catches up
when it comes back, and its chip says how far behind it is. All of it
runs while a writer is awake, and a headless writer on the server is
cut.

- **What starts a sync.** A change made in the app (a tag edit, music
  added), a minute after the last one; the SSD being mounted
  (NSWorkspace's mount notification); app start; and an hourly check of
  Proton, which has no change feed. Each check costs about a minute of
  opening Proton, taking the lease and listing.
- **Music added in MediaLayer** is uploaded to Proton and copied to the
  SSD and the server from the Mac's own copy, so each byte leaves the
  Mac once per place. Music added through Proton's own app works too,
  but appears only at the next check, and each file is then downloaded
  from Proton to be copied on.
- **Tag edits** (milestone 8) are written to a local copy, uploaded to
  Proton and copied to the others from that copy.
- **Deletes** only in the app (rule 5). Anything that vanishes
  elsewhere is put back (rules 3 and 4).
- **Music that appears on the SSD or the server** comes into the
  library on the next sync that sees it (rule 1).
- With a Proton primary, the device's store of audio hashes, keyed today
  by Drive's MD5 (sync.md), needs keying by SHA-1 too, or every pause
  review downloads files again.

### The daughters' library

A Google Drive folder becomes a fourth copy, filled from the Mac and
marked shared, and is shared view-only. Their iOS apps open it as
readers (milestone 6): the catalog and art from the snapshot, Dean's
shared playlists read-only, and their own personal databases on their
phones.
Nothing they do writes to the folder. A track they rated keeps its
rating across Dean's retags, since its file id and audio hash stay.

## Risks

| Risk | What happens | What covers it |
|---|---|---|
| **A missing replica read as empty** (the unplugged SSD) | A full copy to the wrong disk, or an error each sync | The offline fix, first of all steps |
| **A bad import to Proton** | The new primary is missing or damaging files while the old copies are adopted | The download check before the SSD and the server take replacements |
| **A file changed on a secondary outside MediaLayer** | The primary's version put back | The secondary's version kept in its trash for 30 days |
| **Duplicates pile up** | Every retagged copy becomes a version | Counts shown before a source's first import; versions grouped with "pick a keeper" |
| **Junk or damage written into a copy** | It feeds the library | Damaged lossless files are refused; anything else is deleted in the app once, everywhere |
| **A file changed on the primary outside MediaLayer** | Copied out as a real change | Old versions in each replica's trash for 30 days; a wholesale rewrite pauses at the daily limit |
| **A file lost from the primary** | Nothing is removed anywhere | Restored from a copy whose content matches the catalog, after moves are matched (rule 4) |
| **A bad or partial primary listing** | Would look like thousands of files gone, and start a mass restore | Stops the sync with an error |
| **A wrong inventory row** | A wrong tag or art until the file changes | Parser and hash versions; inventories never prove equality or decide removals |
| **An offline replica** | The SSD, unplugged for a month, has files since deleted and lacks new ones | It applies the deletions log and copies what is new when it returns |
| **Two writers at once** (two Macs, or a restored Mac beside the old one) | Conflicting changes | The sync lock, the version id check, and edits that name the version they were made against |
| **References naming tracks that changed** | A playlist item, rating or count whose track was retagged, re-indexed or replaced | File id, whole-file hash (current, then previous), audio hash, then path, refreshed with each catalog version |
| **Installations on different hash or parser versions** | Hashes cleared, rows ignored | sync.md's rule: change these versions only with a release every installation takes, readers included |
| **The Mac lost** | No writer | Any new installation with write access opens the library from any copy and is a writer (new work) |
| **Personal data lost with a device** | Playlists, ratings, counts gone | Shared playlists survive in the library; cloud sync of the personal database is a later milestone |

## What is cut, on purpose

1. **Writers at the same moment.** Any installation with write access
   is a writer, but only one writes at a time, under the lock.
   Readers write only their own personal database.
2. **Two-way sync of edits and deletes.** Adds come from anywhere
   (rule 1); edits and deletes only from a writer.
3. **Copies that differ from the primary.** No sparse copies, no copies
   that keep their own tags because the audio matches. What a copy has
   extra comes into the library instead.
4. **A helper that decides.** Its inventory never stands in for a
   listing when removing or replacing, never proves equality from a
   cached hash, and it never writes music. smb.md's level 2 stays
   shelved.
5. **Indexing from another place's inventory by content hash.** It
   looked like the way to index Proton without reading it, but in this
   setup every byte reaching Proton passes through the Mac, which reads
   the tags as it uploads.
6. **A manager database.** The records stay JSON.
7. **User data shared between people.** Each person's data is theirs;
   the owner's shared playlists are shown to readers read-only. No family
   playlist several people edit.
8. **Importing another library's file ids and other data**
   (consolidate.md step 6). Dean's existing playlists are carried by
   path; nothing else.
9. **Filling a copy from another copy** (consolidate.md section 6). The
   order above doesn't need it.
10. **The daily limit for removals and repairs.** Only deletes made in
    the app travel, and repairs lose nothing. The limit stays for files
    changed on the primary outside the app.
11. **A headless writer on the server.** Syncs happen while a Mac is
    awake. Worth building only if that becomes the bottleneck.

## What changes, in order

1. The offline fix, with "written to" also taken from `repo.json`'s
   members.
2. The sync rules: the deletions log with its apply rule; each file's
   whole-file hash, and the one before its last change, in the catalog; moves matched by
   hash; restoring the primary; `missing` rows kept; a bad primary
   listing stopping the sync.
3. Continuous import from every copy, with versions, "pick a keeper",
   "not duplicates", and the tag-edit rule for tracks sharing audio.
4. `primary_version` and `location` out of the catalog into a local
   file.
5. Personal data out of the catalog: a local file per installation
   with `updated_at` and tombstones, today's playlists moved in,
   ATTACHed by the app; shared playlists published by writers.
6. Several writers: one member list in `repo.json`, following a Make
   Primary, opening a library from any copy as a writer, edits that
   name their catalog version.
7. Content hashes in mlhost's inventory, dropped on any change-time
   move; priming a folder that isn't a library yet; inventories of any
   source; a fresh pass on request; a Mac inventorying a local disk the
   same way.
8. Any kind of source as the first one, which becomes the primary.
9. Compare, import (recording headers as it uploads) and adopt with
   fresh hashes (consolidate.md steps 2 to 4), and the Proton check.
10. Automatic syncs: after a change in the app, on mount, hourly.

Ratings and play counts come with the player work that uses them, into
the personal database from the start. Syncing the personal database to
a cloud is a later milestone.

## Decisions

All decided by Dean on 2026-11-28:

1. **Writers:** any MediaLayer installation connected to the primary
   with write access is a writer, so two Macs can both keep the library
   up to date; they take turns through the sync lock. Readers have no
   write access.
2. **The sync rules**, the four import refinements and the tag-edit
   rule, reduced to one: only the primary is the library, and each sync
   makes every secondary match it path by path, with files at new paths
   brought in (see "Sync rules").
3. **Exact duplicates on a copy** (a fresh whole-file hash equal to a
   library file's) go to its trash without asking.
4. **Personal data** (playlists, ratings, play counts) leaves the
   library's database for a personal database on each device, with cloud
   sync later. Writers can share playlists into the library; readers
   can't, and ratings and counts are never shared.
5. **Priming:** the host helper builds a folder's inventory, hashes
   included, before the folder is attached to a library; content hashes
   are made fresh for anything that removes or replaces a file.
6. **No built-in order:** the first source added becomes the primary.
