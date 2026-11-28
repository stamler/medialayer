# Library data: what is kept where, and who writes it

A design note, written 2026-11-28 for Dean's question: should the one
SQLite index be split into an inventory, a per-installation user
database and the manager's data? It answers that, says what a host
helper (mlhost today, NFS hosts later) can build, takes up Dean's
sync rules (adds come from anywhere, deletes only in the app, losses
put back), lays out his target setup on it, and lists what is cut on purpose. Nothing here is built. The
decisions to make are at the end. An independent review of the first
draft found real gaps (the setup order relied on two things that don't
exist, cached hashes were trusted where the docs refuse them, the user
file had no versioning and leaked listening data to readers); this
version answers them.

## The short answer

Yes, split it, along one rule rather than three topics: **every file
has exactly one writer**. Then nothing has to be merged on write. The
sync lock stays, but only as a guard against a second writer started by
mistake, not as the way writers take turns.

| Piece | What it holds | Written by | Where |
|---|---|---|---|
| **Catalog** | The library: tracks, file ids, tags, art references, audio hashes, the deletions log | The controller, only | Published to every replica as today's snapshots |
| **Inventory**, one per place | Facts about the files at one place: path, size, time, content hashes, audio hash, parsed tags and art | Whoever reads that place fastest: its host helper, or the controller for a disk it can read | `.medialayer/host/` on that place |
| **User data**, one per installation | Playlists, ratings, play counts made on that installation | That installation, only | On the device; owner installations' files also on the replicas |
| **Records** | Which library, which primary, which replicas, each replica's state, which installation is the controller | The controller | `repo.json` and `member.json` on the replicas, `settings.json` on the device (as now) |

Dean's three parts map onto this with two changes:

1. **"Inventory" is two things.** What is physically at a place (a fact
   anyone with fast access can measure) is not what the library is
   (which tracks, under which ids, which playlists point at them).
   Helpers build the first. Only the controller makes the second, mostly
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

- **One writer**, the controller (below). The version id check stays as
  the guard for the abnormal case: a restored backup, or a second
  controller by mistake.
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
2. **Any place, not only the primary.** Today the device loads an
   inventory only from the folder it indexes (`host.go`), and mlhost
   refuses a folder with no `.medialayer` (`hostscan.go`, `ErrNoRepo`).
   A source being compared or adopted needs one too: "Bring its music
   in" writes a small `.medialayer/source.json` there (with the user's
   approval, the only write to the source), which mlhost accepts as
   leave to run.
3. **A fresh pass on request.** For adoption (below), the device asks
   for a pass that starts after its request and waits for that
   inventory, using the probe mechanism smb.md describes for level 2.

Who builds which inventory:

| Place | Built by | How fast |
|---|---|---|
| Linux server over SMB (and NFS later) | mlhost on the server | Disk speed; an hour or two for 483 GB, then only changed files |
| External SSD | The controller, reading it directly | Disk speed, while plugged in |
| A folder on the Mac | The controller | Disk speed |
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

## User data

One SQLite file per installation, ATTACHed to the catalog so the app can
join them (milestone 6's plan). Tables: playlists and their items,
ratings, and per-track play counts with the last time played.

- **One writer.** Each installation writes only its own file.
- **Ready to merge from day one.** Playlist ids are stable (ULIDs, as
  now), each playlist and rating has an `updated_at`, and a removed
  playlist leaves a tombstone. Then a person's view over several of
  their installations' files is computed when reading, never written:
  play counts add up, a rating or a playlist takes its newest version.
  Without these fields from the first version, merging later would need
  a format change.
- **Naming tracks.** Rows keep the file id, the path and `audio_sha256`.
  A row finds its track by file id; failing that, by path with an equal
  audio hash; failing that, by an audio hash that only one track has;
  by path alone only when the row never had a hash. So a different
  recording put at the same path is not taken for the old one, and a
  compilation that shares audio with an album isn't confused with it.
  Rows are often saved before the hash pass has reached a file, so the
  indexer fills `audio_sha256` from an inventory where it can.
- **Versioned like the catalog.** A user file that is published has its
  own version id and parent, checked before it is replaced on a
  replica, so an older copy from a restored Mac doesn't overwrite a
  newer one (the case promote-review.md raises for the index). In
  sync's write order it goes before the snapshot, and `member.json`
  records the user files it matches.
- **Published with a sync, not on every change.** Playlist edits go out
  with the next sync; play counts at most daily.
- **Where it lives.**
  - **Owner installations** (Dean's Mac, his iPhone) publish their own
    file as `.medialayer/users/<installation id>.sqlite` on the primary,
    and the controller copies them on to the replicas with each sync.
    An installation writing only its own file needs no lock. So Dean's
    playlists are backed up three times and follow him between devices.
  - **Reader installations** (his daughters') keep theirs on the
    device. Backing it up to the reader's own cloud comes later
    (milestone 6 already says so).
  - **A shared replica gets no listening data.** On a replica marked
    shared, the controller publishes only the owner's playlists marked
    shared, as `.medialayer/users/shared.sqlite`. Dean's ratings, counts
    and last-played times never reach his daughters' copy.
- **Moving today's playlists.** In this library, the first release with
  this moves the playlist tables into the controller's user file. For
  the new Proton library (below), Dean's current playlists are carried
  by path (`PathPlaylist`, `playlist.go`), then matched by audio hash
  once the new catalog has hashes.

## The controller

One installation per library is **the controller**: the only one that
writes music, the catalog, the records, or deletes anything. For Dean,
the Mac. Every other installation reads the catalog and art from a
replica, plays, and writes only its own user file (on the primary for
an owner installation, on the device for a reader).

This narrows today's design, where any device holding the sync lock
syncs. With one controller these cases go away instead of being
handled:

- two devices' member lists overwriting each other in `repo.json`
  (sync.md, Known gaps);
- playlists carried between devices' indexes when the primary moved on
  (sync.md, "Every copy is a whole library");
- other devices not following a Make Primary;
- two devices with unsaved changes at once.

`repo.json` names the controller (a field `controller`, the
installation id). A second installation that finds it isn't the
controller refuses to write; the sync lock and the version id catch one
that does anyway.

**Losing the Mac.** Every replica holds the whole library, the catalog
and Dean's user files, but taking over needs work that doesn't exist
today (picking a secondary as the music folder is refused, and Choose
Music Folder browses Google Drive only):

- **Take over a library**, from any replica: the new installation
  reads `repo.json` for the primary and members, asks for each one's
  sign-in, takes the newest catalog, records itself as the controller
  (raising the generation, so the old one is refused if it comes back),
  and takes the previous controller's user file as its own.
- The offline fix's "this replica was written to" comes from the
  members in `repo.json`, not only from `settings.json`, so it survives
  the Mac too.

## Sync rules

Dean set these rules on 2026-11-28, after the first version of this
note. They replace its "held for review" design and its "replicas stay
exact copies, extras removed" rule.

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
enter from any place, but only the controller edits or removes it, and
nothing a listing shows missing is ever taken as a delete.

### What counts as new (rule 1)

A file on a copy that isn't the library's file at that path is
classified by its hashes (from that place's inventory, or a fresh read
for a disk):

| The file on the copy | What happens |
|---|---|
| Content hash already in the library, at any path | Not imported: the library has these bytes. If the copy lacks the file at the library's path, it is moved there instead of copied again; otherwise this redundant copy goes to that copy's trash, the one removal made without a person, since no bytes are lost |
| At a library path, with the content the catalog recorded for that path before its last change | A stale copy of a file changed on the primary: replaced, old version to trash, as today |
| Same audio hash as a library track, different content (a retag) | Imported as a **second version** of that track, at its own path (with a suffix if the path is taken). At a library path, the primary's version is then put back there |
| Fails to decode (FLAC, ALAC) | Not imported: damaged. Repaired from the primary. A damaged MP3 or AAC can't be told, and comes in as a version |
| Anything else | Imported at its own path, with a suffix if the path is taken |

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
server comes back into the library as a second version rather than
being lost; and anything that writes music into a copy's folder (a
downloader on the server, say) feeds the library.

To classify a file at a library path, the catalog keeps, for each file,
its content hash as the primary lists it (SHA-1 on Proton, MD5 on
Drive, or the inventory's on SMB) and the one before its last change.

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
- **No good copy anywhere:** the catalog keeps the row, marked
  `missing` (the column exists), and the track shows as lost.
- **A bad listing is not a mass loss.** A Proton listing that fails,
  ends early, or lists far fewer files than the catalog must stop the
  sync with an error, not start a 483 GB restore. The offline fix covers
  the primary too.

### Deleting (rule 5)

The controller records each delete (path, file id, size, content and
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
  are being rewritten wholesale (corruption, ransomware). Repairs of a
  copy lose nothing now (an edited copy comes in as a version), so they
  don't count.
- Every copy holds every library file at the library's path, byte for
  byte, once its sync ends, so any copy can be promoted. A file that a
  copy contributed is in the library by then, not an extra.

## Dean's target, step by step

Proton is the primary, the SSD and the server are copies, the Mac is the
controller. Later a Google Drive copy is shared view-only with his
daughters.

### Getting there

This order needs no Make Primary of 16,800 tracks and none of
consolidate.md's "filling a copy from another copy" (section 6). It
does need two pieces that don't exist: **Proton as a new library's
primary** (Choose Music Folder browses Google Drive only, and a Proton
primary is reached today only by Make Primary), and **mlhost on a
folder that isn't yet a library** (the `source.json` above).

1. **The offline fix** (consolidate.md, section 5): a replica once
   written to that is missing is offline, never empty.
2. **Measure an upload to Proton** of a few hundred tracks from the Mac:
   Proton's per-file overhead (about 14 requests per small file) and its
   known idle upload line make the home upload speed only an upper
   bound. At 100 Mbit/s, 483 GB is about 11 hours; at 20, two days.
3. **Choose which source goes first.** For a track on both with other
   tags, the first source's file takes the library path and the other's
   comes in as a second version to pick a keeper from. Import the
   better-tagged one first, and expect the version count shown before
   the server's import.
4. **Inventory both**: mlhost with content hashes on the server, the
   Mac reading the SSD.
5. **Choose an empty Proton folder** as the music folder.
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
   imports what the library lacks, as files or versions: only those
   cross the 1.5 MB/s link (about 5.4 GB an hour). Rule 2 fills the
   server with the rest, uploaded into the server's site, its fast
   direction.

### Keeping three copies in step

"Always in sync" means: after each change, a sync starts on its own and
reaches every replica that is online; one that is offline catches up
when it comes back, and its chip says how far behind it is. All of it
runs while the Mac is awake, since the Mac is the controller and a
headless one is cut.

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
shared playlists read-only, and their own user files on their phones.
Nothing they do writes to the folder. A track they rated keeps its
rating across Dean's retags, since its file id and audio hash stay.

## Risks

| Risk | What happens | What covers it |
|---|---|---|
| **A missing replica read as empty** (the unplugged SSD) | A full copy to the wrong disk, or an error each sync | The offline fix, first of all steps |
| **A bad import to Proton** | The new primary is missing or damaging files while the old copies are adopted | The download check before the SSD and the server take replacements |
| **A file changed on a copy outside MediaLayer** | The primary's version put back | The edited file comes in as a second version (rule 1), so nothing is lost |
| **Duplicates pile up** | Every retagged copy becomes a version | Counts shown before a source's first import; versions grouped with "pick a keeper" |
| **Junk or damage written into a copy** | It feeds the library | Damaged lossless files are refused; anything else is deleted in the app once, everywhere |
| **A file changed on the primary outside MediaLayer** | Copied out as a real change | Old versions in each replica's trash for 30 days; a wholesale rewrite pauses at the daily limit |
| **A file lost from the primary** | Nothing is removed anywhere | Restored from a copy whose content matches the catalog, after moves are matched (rule 4) |
| **A bad or partial primary listing** | Would look like thousands of files gone, and start a mass restore | Stops the sync with an error |
| **A wrong inventory row** | A wrong tag or art until the file changes | Parser and hash versions; inventories never prove equality or decide removals |
| **An offline replica** | The SSD, unplugged for a month, has files since deleted and lacks new ones | It applies the deletions log and copies what is new when it returns |
| **A second controller** (a restored Mac) | Two writers | `controller` in `repo.json`, the sync lock and the version ids of catalog and user files |
| **User files naming tracks that changed** | A playlist item whose track was re-indexed or replaced | The lookup order above |
| **Installations on different hash or parser versions** | Hashes cleared, rows ignored | sync.md's rule: change these versions only with a release every installation takes, readers included |
| **The Mac lost** | No controller | Take over from any replica (new work) |

## What is cut, on purpose

1. **More than one writer of music, catalog or records.** One
   controller. Other installations write only their own user file.
2. **Two-way sync of edits and deletes.** Adds come from anywhere
   (rule 1); edits and deletes only from the controller.
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
11. **A headless controller on the server.** Syncs happen while the Mac
    is awake. Worth building only if that becomes the bottleneck.

## What changes, in order

1. The offline fix, with "written to" also taken from `repo.json`'s
   members.
2. The sync rules: the deletions log with its apply rule; each file's
   content hash, and the one before, in the catalog; moves matched by
   hash; restoring the primary; `missing` rows kept; a bad primary
   listing stopping the sync.
3. Continuous import from every copy, with versions, "pick a keeper",
   "not duplicates", and the tag-edit rule for tracks sharing audio.
4. `primary_version` and `location` out of the catalog into a local
   file.
5. User data out of the catalog: a versioned file per installation with
   `updated_at` and tombstones, today's playlists moved in, owner
   installations' files published through the primary, a shared-only
   file for shared replicas, ATTACHed by the app.
6. `controller` in `repo.json`, and refusing writes from any other
   installation.
7. Content hashes in mlhost's inventory, dropped on any change-time
   move; inventories of any source (`source.json`); a fresh pass on
   request; the controller inventorying a local disk the same way.
8. Proton as a new library's primary.
9. Compare, import (recording headers as it uploads) and adopt with
   fresh hashes (consolidate.md steps 2 to 4), and the Proton check.
10. Automatic syncs: after a change in the app, on mount, hourly.
11. Take over a library from any replica.

Ratings and play counts come with the player work that uses them, into
the user file from the start.

## Decisions for Dean

1. **One controller per library** (the Mac) for music, catalog, records
   and deletes, with his other installations, the iPhone included,
   writing only their own user files to the primary so his playlists
   follow him. I recommend it.
2. **Decided:** the five sync rules, the four import refinements and
   the tag-edit rule (see "Sync rules").
3. **Decided:** redundant byte-identical copies on a copy go to its
   trash without asking (a fresh whole-file hash equal to the library
   file's), the one removal made without a person, since no bytes are
   lost.
4. **What readers see of his data:** only playlists he marks shared, no
   ratings or play counts. I recommend it.
5. **Hashes:** content hashes are for comparing and adopting, made fresh
   at adoption, never cached proof; Proton is checked by downloading
   before the old copies follow it. I recommend it; it costs a fresh
   read of the SSD and a fresh mlhost pass at adoption.
6. **The order of getting there:** an empty Proton primary filled from
   the SSD, then the server's extras, rather than consolidate.md's
   server-first plan. I recommend it, but it needs Proton as a new
   primary and mlhost on a non-library folder built first, and the
   better-tagged of the SSD and the server imported first.
