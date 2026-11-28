# Library data: what is kept where, and who writes it

A design note, written 2026-11-28 for Dean's question: should the one
SQLite index be split into an inventory, a per-installation user
database and the manager's data? It answers that, says what a host
helper (mlhost today, NFS hosts later) can build, takes up Dean's
follow-up (adds automatic, deletes manual), lays out his target setup
on it, and lists what is cut on purpose. Nothing here is built. The
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

## Replicas stay copies of the primary

Dean's idea: replicate everywhere, delete nothing, skip a copy when the
file already exists by audio hash. As a one-time import that is right.
As the steady state it breaks what makes a replica a backup you can
switch to.

- **Same audio is not the same file.** Two files with one audio hash
  can differ in tags and art. A replica that kept its own version
  "because the audio matches" is not a copy: promoting it changes the
  library's tags.
- **Deleting nothing, ever,** leaves each replica with files the
  library doesn't have and no record of why, so every Make Primary would
  need a review of its extras. The next section keeps the useful part
  (nothing leaves a replica unless a person chose it) with a record of
  what is held.
- **What is kept instead.** Every replica holds exactly the primary's
  files, byte for byte, plus `.medialayer`, apart from items held for
  review, which `member.json` lists (next section). Hashes make import and adoption cheaper, never
  looser: an import lists a file whose audio is already in the library
  instead of copying it, and adoption keeps a file proven equal by fresh
  hashes instead of copying it again, and a file moved on the primary is
  moved on the replica rather than copied again (a requirement of the
  next section, and a large saving on the server).

## Adds are automatic, deletes are manual

Dean's follow-up: sync only ever adds on its own, and every delete is
manual. I'd adopt it, with "manual" meaning **a person chose it in the
app**, not "a listing showed it gone". It is not a pure
simplification: replicas become an exact copy plus a listed set of held
files, and a few rules are needed to keep that safe.

| Change found | Today | With this rule |
|---|---|---|
| A file new on the primary | Copied | Copied |
| A file edited in MediaLayer (a tag edit) | Replaced, old version to trash, not counted | Replaced, old version to trash, not counted |
| A file deleted in MediaLayer | Removed to trash on each replica, counted | Removed to trash on each replica, not counted: the delete in the app was the manual act |
| A file moved or renamed on the primary outside MediaLayer | The new path copied, the old one removed | Matched by content or audio hash and moved on the replica, not copied again |
| A file gone from the primary, not deleted in MediaLayer (Proton's app, a shell, a failed disk) | Removed to trash, counted, pauses past the limit | **Held**: removed nowhere, listed as "Gone from the primary" |
| A file changed outside MediaLayer, on the primary or on a replica | Replaced from the primary, old to trash, counted unless only retagged | The same, as today |
| A file only on a replica | Removed to trash, counted | **Held**, listed as an extra on that replica |

So the daily limit stays, for replacements not made in the app (the
guard against files being rewritten wholesale, by corruption or
ransomware), and removals no longer count against it. One thing to say
plainly: a file edited directly on a replica (a retag in a shell on the
server) is put back from the primary, its edit kept only in that
replica's trash for 30 days, as today.

**The deletions log.** The controller records each delete it makes
(path, file id, size, content and audio hash, time) in a log in the
catalog. A replica's file is removed only when the log names it, the
primary's current listing lacks that path, and the replica's file is
the logged version (size and hash), so a delete followed by a new file
at the same path never removes the new one from a disk that returns
later. The log is never pruned by age; each replica's `member.json`
records how far into it it has applied. An in-app delete is also
MediaLayer's first removal of music from the primary: it goes to the
primary's `.medialayer/trash`, under the merge's per-file lease rules
(consolidate.md).

**Held files.**
- Each replica's `member.json` lists its held files with their paths
  and hashes, not just a count, so Make Primary and readers can tell
  them from library files. `synced` then means "matches this snapshot,
  plus these held files".
- The catalog keeps a row for a file gone from the primary, marked
  `missing` (the column exists) with its file id, and publishes as
  usual; playlists and ratings still point at it.
- One review list, "Gone from the primary", offers Delete everywhere
  (which writes the log) and Restore. Restore copies the file back to
  the primary under its old file id, from the replica whose copy
  matches the catalog row's hash, preferring the SSD (a mass restore
  from the server is about 90 hours at 1.5 MB/s), with a Restore all.
- Make Primary to a replica with held files is refused until they are
  resolved, so files removed outside the app don't come back under new
  ids.
- An adoption's approval writes log entries for the files it removes,
  so it is the manual act for them.
- Held files take space on every replica, so the chip shows how many
  and how large.

**A bad listing is not a mass loss.** A Proton listing that fails, ends
early or names another folder must stop the sync with an error, not
produce 16,800 held items. The offline fix covers the primary too: a
primary missing its `.medialayer`, or listing far fewer files than the
catalog holds, is an error to show, not a change to apply.

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
3. **Choose which source's tags win.** For a track on both with other
   tags, the first source imported wins unless the review picks the
   other. Import the better-tagged one first.
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
9. **Import from the server** only what the SSD lacked. The compare is a
   join of the two inventories, and only the server's extra files cross
   the 1.5 MB/s link (about 5.4 GB an hour).
10. **Adopt the server as a copy**, after a fresh mlhost pass requested
    for it. Files equal by fresh hash are kept. Files that differ (a
    retagged copy, say) are replaced from the Mac's side, an upload
    into the server's site, its fast direction, with the server's
    versions in its trash, after one approval of the list.

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
- **Deletes** as above: made in the app, they travel; anything else is
  held.
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
| **A file changed on a replica outside MediaLayer** | Put back from the primary at the next sync | Its old version goes to trash first |
| **A file changed on the primary outside MediaLayer** | Copied out as a real change | Old versions in each replica's trash for 30 days; a wholesale rewrite pauses at the daily limit |
| **A file lost from the primary** | Nothing is removed anywhere | Held, and restorable from a copy |
| **A bad or partial primary listing** | Would look like thousands of files gone | Stops the sync with an error |
| **A wrong inventory row** | A wrong tag or art until the file changes | Parser and hash versions; inventories never prove equality or decide removals |
| **An offline replica** | The SSD, unplugged for a month, has files since deleted and lacks new ones | It applies the deletions log and copies what is new when it returns |
| **A second controller** (a restored Mac) | Two writers | `controller` in `repo.json`, the sync lock and the version ids of catalog and user files |
| **User files naming tracks that changed** | A playlist item whose track was re-indexed or replaced | The lookup order above |
| **Installations on different hash or parser versions** | Hashes cleared, rows ignored | sync.md's rule: change these versions only with a release every installation takes, readers included |
| **The Mac lost** | No controller | Take over from any replica (new work) |

## What is cut, on purpose

1. **More than one writer of music, catalog or records.** One
   controller. Other installations write only their own user file.
2. **Two-way sync of music.** Never. Music enters by the controller,
   from a local folder or by importing a source.
3. **Replicas that differ from the primary**, apart from held items. No
   sparse copies, no copies that keep their own tags because the audio
   matches, no "delete nothing" as the steady state.
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
10. **The daily limit for removals.** Only deletes made in the app
    travel; anything else gone is held. The limit stays for unplanned
    replacements.
11. **A headless controller on the server.** Syncs happen while the Mac
    is awake. Worth building only if that becomes the bottleneck.

## What changes, in order

1. The offline fix, with "written to" also taken from `repo.json`'s
   members.
2. The deletions log with its apply rule, held files in `member.json`,
   `missing` rows kept in the catalog, moves matched by hash, the
   review list with Restore, and a bad primary listing stopping the
   sync.
3. `primary_version` and `location` out of the catalog into a local
   file.
4. User data out of the catalog: a versioned file per installation with
   `updated_at` and tombstones, today's playlists moved in, owner
   installations' files published through the primary, a shared-only
   file for shared replicas, ATTACHed by the app.
5. `controller` in `repo.json`, and refusing writes from any other
   installation.
6. Content hashes in mlhost's inventory, dropped on any change-time
   move; inventories of any source (`source.json`); a fresh pass on
   request; the controller inventorying a local disk the same way.
7. Proton as a new library's primary.
8. Compare, import (recording headers as it uploads) and adopt with
   fresh hashes (consolidate.md steps 2 to 4), and the Proton check.
9. Automatic syncs: after a change in the app, on mount, hourly.
10. Take over a library from any replica.

Ratings and play counts come with the player work that uses them, into
the user file from the start.

## Decisions for Dean

1. **One controller per library** (the Mac) for music, catalog, records
   and deletes, with his other installations, the iPhone included,
   writing only their own user files to the primary so his playlists
   follow him. I recommend it.
2. **Replicas are exact copies**, apart from items held for review;
   "delete nothing" is the rule for imports only. I recommend it.
3. **Adds automatic, deletes manual:** only a delete made in the app
   reaches the replicas; a file gone any other way is held on every
   replica and can be restored. The cost: replicas become an exact copy
   plus a listed held set, Make Primary waits until that is reviewed,
   edits made directly on a replica are still put back from the
   primary, and the daily limit stays for replacements not made in the
   app. I recommend it.
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
