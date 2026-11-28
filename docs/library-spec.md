# Library spec: sources, sync, the inventory helper and personal data

Status: draft for Dean's review, 2026-11-28. Nothing here is built.

This spec says how MediaLayer manages one library that is kept in
several places. It covers:

- the sources of a library, and how MediaLayer keeps them the same;
- who can change the library;
- the inventory helper (`mlhost`);
- the split of the old single database into a catalog, inventories and
  personal databases.

Part 1 gives the principles and the terms. Part 2 gives the rules, in
the order that MediaLayer applies them. Part 3 gives the details of how
MediaLayer applies them. Part 4 gives the other rules (access, data,
tags, references, the helper). Part 5 says what each database holds.
Part 6 is the inventory helper. Part 7 gives scenarios and what
MediaLayer does in each. Part 8 lists the open questions.

Where this spec and an older document do not agree, this spec is
correct. The reasons for the decisions are in
[library-data.md](library-data.md). The sync mechanics that do not
change (the sync lock, snapshots, snapshot ids, the write order) are in
[sync.md](sync.md).

## Part 1. Principles and terms

### Principles

The principles are in order of priority. When two principles do not
agree, the first one wins.

1. **Keep the data.** MediaLayer does not lose music or anything that a
   person made. It removes a file only when a person asked for it, or
   when the reason is completely clear. Before it removes or replaces a
   file, it moves the old file to trash.
2. **The Primary is the library.** One source is the Primary. The music
   on the Primary is the library. Every other source is a copy of the
   Primary. To keep a change, make it on the Primary.
3. **Storage access is the only access control.** MediaLayer has no
   user accounts. A person who can write to the Primary controls the
   library. A person who can only read has every other feature, and
   keeps their own data on their own device.
4. **Every copy is complete.** After a sync, each source holds the
   full library. A writer can make any complete source the Primary.
5. **No surprises.** MediaLayer does the same thing each time in the
   same situation. It tells the user about each change that it makes
   without a request.

Principle 2 can surprise a user. For example, MediaLayer undoes a retag
that a person made directly on a Secondary (S13). Principle 2 wins,
and MediaLayer tells the user what it undid and where the old file is.

### Terms

| Term | Meaning |
|---|---|
| **Source** | A folder on a storage service that holds a copy of the library. Examples: a Proton Drive folder, a Google Drive folder, a folder on an SMB share, a folder on an external disk. |
| **Primary** | The one source that is the library. |
| **Secondary** | A source of the library that is not the Primary. |
| **Library** | The files on the Primary, and the catalog that describes them. |
| **Library path** | The path of a current catalog entry. A path in the deletions log is not a library path. |
| **Installation** | MediaLayer on one device. |
| **Writer** | An installation that can write to the Primary. |
| **Reader** | An installation that cannot write to the Primary. |
| **Catalog** | The library's database. It describes each file on the Primary. |
| **Snapshot** | One saved state of the catalog, with its own id. |
| **Inventory** | A database of facts about the files in one folder. |
| **Personal database** | The playlists, ratings and play counts of one installation. |
| **Host helper** | `mlhost`. A program that runs on the computer that holds a folder, and writes the inventory of the folder. |
| **File hash** | A SHA-256 hash of all the bytes of a file. Two files with the same file hash are the same file. MediaLayer also keeps each file's SHA-1 and MD5, because Proton lists SHA-1 and Google Drive lists MD5. |
| **Audio hash** | A hash of the audio in a file, without its tags. A retag does not change the audio hash. |
| **File history** | All the hashes that one library file had: the current ones and each set before a change. |
| **Duplicates** | Two or more library files with the same audio hash. |
| **Trash** | The folder `.medialayer/trash` in each source. |
| **Deletions log** | The list in the catalog of each file that a writer deleted in MediaLayer. |
| **Sync** | A job that a writer does. It updates the catalog from the Primary, and makes each Secondary the same as the Primary. |
| **Missing track** | A catalog entry whose file is not on any source. |
| **Settled** | A file is settled when two listings, 2 minutes or more apart, show the same size and time. |

## Part 2. The rules

MediaLayer applies these rules in order. For each file, the first rule
that matches decides what happens, and MediaLayer does not look at the
rules after it. The order is part of the rule: it is what makes the
outcome right. Part 3 says how MediaLayer does each step.

### Always

These rules come first, for every sync and every file.

1. **If the Primary looks wrong, stop.** The Primary cannot be listed,
   its listing stops early, it shows far fewer files than the catalog,
   or its `.medialayer` is missing or for another library. MediaLayer
   changes nothing, and shows the problem.
2. **If a Secondary is missing, it is offline.** MediaLayer skips it. It
   never fills another folder or disk with the same name.
3. **If a file is still changing, wait.** MediaLayer uses a file only
   when it is settled.
4. **Before a file is removed or replaced, move it to trash.** It stays
   there 30 days. This is true on the Primary and on each Secondary.
5. **Only a writer changes the library.** A reader writes nothing to any
   source. One writer works at a time.
6. **Only a delete in MediaLayer deletes music.** No other event deletes
   a track from the library.

### Files on the Primary

The Primary decides what the library is. MediaLayer applies these rules
to the Primary first, and then makes each Secondary match.

7. **A file deleted in MediaLayer:** remove it from each Secondary.
8. **A file that moved on the Primary:** move it on each Secondary.
   The track keeps its id.
9. **A file that is gone from the Primary:** copy it back from a
   Secondary or a trash. If no copy exists, keep the track as missing.
10. **A file on the Primary that is damaged:** replace it with the good
    copy from a Secondary. If no Secondary has one, leave it and mark it
    damaged.
11. **A file that changed on the Primary:** copy it to each Secondary.
    If the audio of many files changed outside MediaLayer, pause and ask
    first.
12. **Every other file on the Primary:** if it is new, add it to the
    library. Copy it to each Secondary that does not have it.

### Files on a Secondary

13. **The same file as the Primary has at that path:** do nothing.
14. **A file at the path of a missing track:** keep it. Restore it to
    the Primary if it is the track's file. Otherwise offer it to the
    writer.
15. **A copy of a library file, at another path or in an older
    version:** move it to its library path if that path is empty, and
    update it. Otherwise move it to trash.
16. **A copy of a deleted file:** move it to trash.
17. **A different file at a library path:** if it is a different
    recording, import it under a new name first. Then replace it with
    the Primary's file.
18. **A file at a new path:** import it to the Primary, then copy it to
    each other Secondary. Leave it, and list it as "not imported",
    when it is not a file that MediaLayer can import, when it has the
    audio of a deleted track that no library file has, or when imports
    are off for that Secondary. When a library file has the same audio,
    the import is a duplicate of that track. Pause and ask when one Secondary has more than 100 files
    to import in a day.

### Why the order matters

- Rule 1 comes before all others, so a broken listing of the Primary
  can never look like thousands of deleted files.
- Rule 7 comes before rules 8 and 9, so a file deleted in MediaLayer is
  never copied back.
- Rule 8 comes before rule 9, so a moved file is not seen as lost and
  copied back to its old path.
- Rule 10 comes before rule 11, so damage on the Primary is not copied
  to the Secondaries.
- Rule 13 comes before rules 15 and 16, so the file that a writer kept
  is never removed when an identical file is deleted.
- Rule 14 comes before rule 17, so the only other copy of a missing
  track is never replaced.
- Rules 15 and 16 come before rule 18, so an old copy or a deleted file
  on a Secondary is never imported again.

## Part 3. How MediaLayer applies the rules

This part gives the details behind the rules. The rules in Part 2 stay
the same if these details change.

### Kinds of file

- An **audio file** is a FLAC, ALAC, MP3 or AAC file that MediaLayer can
  read. A FLAC or ALAC file must also decode.
- A **companion file** is an image, `.cue`, `.log` or `.txt` file in a
  folder that has audio files.
- MediaLayer imports only audio files and companion files (rule 18).
- MediaLayer ignores each folder whose name starts with `.medialayer` at
  the top of a source, and operating-system files such as `.DS_Store`.

### Settled files (rule 3)

A file is settled when two listings, 2 minutes or more apart, show the
same size and time.

### Recognising a file

- **The same file** has the same file hash.
- **File history.** Each time the Primary's file at a library path
  changes, MediaLayer keeps the old hashes in that file's history. So a
  file on a Secondary with an older hash is a known old copy (rule 15),
  even after several changes and a move.
- **A deleted file** is a file whose hash is in the history of an entry
  in the deletions log (rules 7 and 16).
- **A move** (rule 8) is one of these: the file's current hash is at a
  new path on the Primary; or exactly one file is gone and exactly one
  new file has its audio hash (a move and a retag).
- **Damage** (rule 10) is a FLAC or ALAC file that changed and does not
  decode now.
- **A different recording** (rule 17) is an audio file whose audio no
  library file has, and no deleted file had.
- **Fresh facts.** MediaLayer removes or replaces a file only because of
  the source's own listing, or a hash made after the file last changed,
  by MediaLayer or by a fresh helper scan. An empty or unknown hash
  means "read the file".

### Matching names

MediaLayer compares paths in Unicode NFC form. On a source that ignores
case, it also ignores case. When a Secondary has two files whose names
differ only by case, and the Primary ignores case, the file whose name
matches the library path exactly is at the library path. The other file
is at a new path, and gets a suffix if it is imported.

When the Primary has two paths that a Secondary cannot hold at the same
time (for example, `Track.flac` and `track.flac` on a case-sensitive
Primary, with a Secondary that ignores case), MediaLayer asks the writer
to rename one on the Primary. Until then, the Secondary shows
"incomplete".

### Copying, importing and restoring

- **To copy** a file to a Secondary (rules 11, 12, 18), MediaLayer
  writes it under a temporary name and renames it when it is complete.
- **To import** a file (rules 17, 18), MediaLayer copies it to the
  Primary at the same path (or with a suffix such as `Track (2).flac`),
  adds it to the catalog, and copies it to each other Secondary.
- **To restore** a file (rules 9, 10, 14), MediaLayer copies it to the
  Primary from the fastest source, with the same file id. When the copy
  is an older version, MediaLayer says so.
- **Check.** After MediaLayer copies a file to the Primary, the hash
  pass reads it back and checks its hash. Until it passes, MediaLayer
  does not use it to replace a different file on a Secondary.

### Pauses

- **The first sync of a new Secondary.** MediaLayer shows what each rule
  will do (counts and size), and the writer starts the sync or cancels.
- **Changes on the Primary (rule 11).** When more than 30 files (or 5%
  of the library, if that is smaller) change their audio outside
  MediaLayer in 24 hours, or become damaged, MediaLayer pauses before it
  changes the Secondaries. The writer chooses "Accept the changes" or
  "Restore the old files". Changes made in MediaLayer and retags do not
  count. Other files continue to sync.
- **Imports (rule 18).** After the first sync, when one Secondary has
  more than 100 files to import in one day (rules 17 and 18 together),
  MediaLayer pauses that Secondary's imports and asks.

### Files listed as "not imported"

A writer can act on each one: Import, or Move to trash.

### Missing tracks (rules 9 and 14)

MediaLayer keeps the entry of a missing track, with its file id, and
never removes it because of time. When any source or trash has a file
whose hash is in the track's history, MediaLayer restores it. Until
then, a writer can:
- **Relink** the entry to another library file. The catalog records
  that the old file id now means the other file, so each personal
  database follows it.
- **Delete** the entry. This writes the deletions log.

When another library file has the same audio hash, MediaLayer offers to
relink the missing entry to it. It always asks first.

### Trash (rule 4)

After 30 days, MediaLayer deletes a file from trash. This is the only
time that MediaLayer deletes a file permanently. It does not delete a
file from trash when the device clock does not agree with the source's
own time, or when the file's hash is in the history of a missing track.

### One writer at a time (rule 5)

A writer holds the sync lock (sync.md) while it changes the library.
Before each change, it takes the newest snapshot from the Primary. A
reader opens each source through a wrapper that refuses every write,
also when the storage service allows the write.

## Part 4. Other rules

These rules do not depend on order.

### Sources and the Primary

**R1. The first source is the Primary.** When a writer makes a new
library, the first source that it adds becomes the Primary. MediaLayer
does not move or remove anything in it. Each later source becomes a
Secondary.

**R2. Write each change to the Primary first.** A tag edit, an import,
a restore or a delete starts on the Primary. Then the sync makes the
Secondaries match.

**R3. Make Primary.** A writer can make a complete Secondary the
Primary (sync.md, "Promoting a Secondary"). The old Primary becomes a
Secondary. Its first sync as a Secondary pauses before any removal or
replacement, as today.
- MediaLayer refuses when the Secondary is incomplete.
- When the Secondary has files listed as "not imported", MediaLayer
  shows them first. The writer imports each group or moves it to trash.
  Files that stay would become library files on the new Primary.
- MediaLayer warns before it continues in two cases: other people can
  write to the new Primary (for example, a shared folder), or another
  writer has no sign-in for it. That writer becomes a reader of the
  library until it signs in.
- The switch succeeds only when the old Primary takes the new
  `repo.json`. If it cannot, MediaLayer undoes the switch, and the old
  Primary stays the Primary. This differs from sync.md step 12 today,
  which continues. Other writers follow a switch when they read the old
  Primary's `repo.json`.

**R4. Replace a lost Primary.** When the Primary is lost (the account
is closed, the disk failed), a writer can choose "Replace the lost
Primary" and pick a complete Secondary. MediaLayer raises the
generation and writes the new `repo.json` to each source that it can
reach. Another writer that finds a newer generation on a Secondary, and
cannot reach the old Primary, asks its user before it follows. If the
old Primary comes back, MediaLayer handles it as a Secondary whose
first sync pauses before any removal or replacement.

**R5. Remove a source.** When a writer removes a Secondary, MediaLayer
stops syncing it and takes it off the list of sources. It does not
remove any file from it.

### Access

**R6. Write access to the Primary makes a writer.** An installation
that can write to the Primary is a writer. Any device can be a writer.
All other installations are readers.

**R7. Only writers change the library.** Only a writer can add or
remove sources, edit tags, delete, import, share playlists, make a
source the Primary, or resolve duplicates and missing tracks.

**R8. A writer syncs only the Secondaries it can write to.** When a
writer cannot reach a Secondary, or has no sign-in for it, it skips the
Secondary. Another writer that can reach it syncs it.

**R9. Readers get every other feature.** A reader can browse, search,
play, keep music offline, make playlists, rate tracks, and count plays.
It keeps these in its personal database on its own device.

**R10. A writer that cannot reach the Primary acts as a reader.** It
plays from any source it can reach. It does not edit, import or delete
until it can reach the Primary.

**R11. Long jobs share the lock.** A long job, for example a first
import of 483 GB, checks every 10 minutes for another writer that waits
for the lock. When one waits, the job releases the lock, waits until
the other writer is done, takes the lock again, takes the newest
snapshot, and continues.

### Data

**R12. Keep the library's data in three kinds of database.**
- The **catalog** describes the library. Only a writer that holds the
  sync lock changes it. MediaLayer copies each snapshot to every
  source.
- An **inventory** describes the files in one folder. The host helper,
  or an installation that can read the folder directly, writes it.
- A **personal database** holds the playlists, ratings and play counts
  of one installation. Only that installation changes it. It stays on
  the device.

Part 5 gives the contents of each.

**R13. Never delete personal data.** MediaLayer does not remove a
playlist, a rating or a play count unless the user removes it. When a
track leaves the library, each reference to it stays, and shows the
track as not available.

### Duplicates and tags

**R14. Keep duplicates apart.** MediaLayer never joins two duplicates
on its own. It shows them together, with the action "Pick a keeper".
When a writer picks one, MediaLayer deletes the others. A writer can
mark two files as "Not duplicates", for example an album track and the
same recording on a compilation. Then MediaLayer stops asking.

**R15. Carry a tag edit field by field.** When a writer edits a tag,
MediaLayer writes the edit to that file. For each other library file
with the same audio hash, MediaLayer changes a field only if that field
had the same value as in the edited file before the edit. MediaLayer
asks before it changes another file. The default answer is yes for
duplicates on the same album. For other albums, MediaLayer asks with no
default.

### References

**R16. Find a track by its id first.** Each reference to a track (a
playlist item, a rating, a play count) keeps the library id, the file
id, the file hash, the audio hash and the path. MediaLayer finds the
track in this order:
1. The file id, or the file id that the catalog relinked it to.
2. The file hash, in the file history of each library file.
3. The audio hash, when only one library file has it.
4. The audio hash and the path, when more than one file has the audio
   hash.
5. The path, only when the reference has no hash.

After step 1 or 2, MediaLayer updates the reference with the track's
current id, hashes and path. After step 3, 4 or 5, MediaLayer shows the
match as a guess, and updates the reference only when the user accepts
it. When no step finds the track, MediaLayer asks the user, or shows the
track as not available.

### The inventory helper

**R17. The helper gives facts, not decisions.** An inventory makes work
faster. MediaLayer never removes or replaces a file only because of an
inventory row (Part 3, "Fresh facts").

**R18. The helper writes only its own folder.** It writes only
`.medialayer/host/` in the folder that it scans. It never writes music.

**R19. The helper can prime a folder.** It can scan a folder before the
folder is a source of a library. A folder that has only
`.medialayer/host/` is not a library.

**R20. Use an inventory row only when it is current.** MediaLayer uses a
row only when the path, size and time match its own listing, and the
helper used the same parser and hash versions as MediaLayer.

### Sync times and reports

**R21. Sync on its own.** A writer syncs:
- one minute after the last change made in MediaLayer;
- when a Secondary disk is connected;
- when the app starts;
- every hour, to find changes made outside MediaLayer.

Readers do not sync. They load a new snapshot when the app starts and
on Update Library.

**R22. Report each change.** The activity list shows each import,
restore, replacement, move and removal that MediaLayer made without a
request, with the source and the reason. Each source shows its state:
synced, syncing, paused, offline, incomplete, or not reachable from this
device.

## Part 5. The data

### The catalog

The catalog is a SQLite database on the Primary. Each change makes a
new snapshot, with a new id. MediaLayer copies each snapshot to
`.medialayer/snapshots/` on each source (sync.md).

| Contents | Notes |
|---|---|
| Files: file id, path, size, time, file hashes (SHA-256, SHA-1, MD5), audio hash, format, duration | SHA-1 and MD5 let MediaLayer match a file on Proton or Google Drive from the listing, without reading it. A Proton SHA-1 was recorded by the uploading app, so MediaLayer trusts it only for files that it uploaded and checked (Part 3, "Check"). An empty hash means "read the file". |
| File history: each past set of file hashes, by file id | Rules 8, 15 and 16, R16 |
| Tags and art references | |
| Missing marks | Rule 9 |
| Relinks: old file id to new file id | Missing tracks |
| The deletions log: path, file id, file history, audio hash, time, device | Never shortened |
| "Not duplicates" marks | R14 |

The catalog does not hold playlists, ratings, play counts, or facts
about one device. The file hash is known after the hash pass reads the
file. Until then, MediaLayer uses the SHA-1 or MD5 from the listing, or reads the file
when it must decide (Part 3, "Fresh facts").

Shared playlists are in a separate file,
`.medialayer/playlists/shared.sqlite`. A writer changes it under the
sync lock. MediaLayer copies it to each source.

### Records

- `repo.json` on each source: the library id, the Primary, the
  generation, and the list of sources. Writers change the list under
  the sync lock.
- `member.json` on each Secondary: its state (copying, synced, paused,
  incomplete), the snapshot it holds, and how far into the deletions
  log it is.
- `settings.json` on each device: that device's sign-ins and choices.

Readers can read the catalog and `repo.json`. So a reader can see the
list of sources, the deletions log and the names of the writers'
devices. A reader cannot see sign-ins, personal databases, or anything
that only a writer's device holds.

### Inventories

An inventory is the file `.medialayer/host/inventory.json.gz` in a
folder. Each row is one file: path, size, time, the helper's change
marker, file hashes (SHA-256, SHA-1, MD5), audio hash, and the parsed
tags and art. Art is in `.medialayer/host/art/`.

### Personal databases

A personal database is a SQLite file on the device. MediaLayer attaches
it to the catalog, so the app can show both together. One personal
database can hold data for more than one library.

| Contents | Notes |
|---|---|
| Playlists and their items | Each item is a reference (R16) |
| Ratings, play counts, last played | Each is a reference |
| `updated_at` on each playlist and rating, and a mark for each removed playlist | So a later sync between a person's devices can join them |

Syncing a personal database to a cloud that the user chooses is a
later milestone.

## Part 6. The inventory helper

`mlhost` runs on the computer that holds a folder: a Linux server with
systemd, a NAS in Docker, or a Mac with launchd. It reads the folder at
disk speed. It is the same program for SMB and NFS shares, because it
reads its own disk.

What it does:
1. It scans the folder when it starts, and then every 10 minutes. It
   reads only the files whose size, time, inode or change time changed.
2. For each media file, it writes the parsed tags and art, the audio
   hash, and the file hashes. It reads each file one time for all of
   them.
3. When the change time of a file moves, it removes the file hashes of
   that file until it reads the file again.
4. When MediaLayer asks for a fresh scan, it scans, and writes an
   inventory that is newer than the request.

What it does not do:
- It does not write music, or any file outside `.medialayer/host/`.
- It does not decide what to copy, replace or remove.
- It does not need a network port or a password.

Why it matters: Dean's server has 483 GB and sends at 1.5 MB/s. To read
each file over the network takes about 90 hours. The helper reads them
on the server in one or two hours. When Dean adds the server as a
source, MediaLayer compares it with the library from the inventory. It
copies over the network only the files that it must.

## Part 7. Scenarios

Each scenario gives the situation, what MediaLayer does, and the rules
that apply. "Dean's setup" is: Proton is the Primary, an external SSD
and a Linux server are Secondaries, his Mac is the writer, and a Google
Drive Secondary is shared with his daughters.

### Setting up

**S1. An empty folder as the first source.** Dean adds an empty Proton
folder first.
- MediaLayer makes Proton the Primary and makes an empty catalog.
- R1.

**S2. A full folder as the first source.** A different user adds a
server folder with 10,000 tracks first.
- The folder becomes the Primary. MediaLayer indexes it. It does not
  move, rename or remove any file in it.
- R1.

**S3. A full disk as the second source.** Dean adds the SSD (483 GB) to
the empty Proton library.
- MediaLayer reads the SSD on the Mac. It shows: "16,800 files to
  import, 483 GB." Dean starts the sync.
- MediaLayer uploads each file to Proton. It reads the tags during the
  upload, so it does not read the files from Proton again.
- After each upload, the SSD already has the file at its library path.
  Nothing is copied to the SSD.
- The upload takes as long as the home upload speed allows (about
  11 hours at 100 Mbit/s). It continues after an interruption.
- The hash pass then reads the files back from Proton and checks them.
- Rules 18 and 13, R11, Part 3 "Check".

**S4. The server, primed by the helper.** Dean runs `mlhost` on the
server before he adds it. The server has most of the same music. Some
albums have other tags. Some albums are in other folders. 200 albums
are only on the server. Some tracks at the same name are a different
rip.
- MediaLayer asks the helper for a fresh scan, and compares the server
  with the catalog. It shows the counts for each rule.
- Same path, same bytes: nothing (rule 13).
- Same bytes at another path: MediaLayer moves the file on the server
  to the library path. Nothing crosses the network (rule 15).
- Same path, other tags: MediaLayer replaces the server's file with
  the library's file. The server's file goes to the server's trash
  (rule 17). These copies go into the server, which is its fast
  direction.
- Same path, a different rip (different audio): MediaLayer imports the
  server's rip as `Track (2).flac`, and then replaces the file at the
  library path (rule 17). No recording is lost.
- New path, new audio: MediaLayer imports the file, at 1.5 MB/s
  (rule 18).
- New path, audio that the library has, other tags: MediaLayer imports
  it as a duplicate (rule 18).
- MediaLayer replaces files on the server only with files that passed
  Part 3, "Check".
- Rules 13, 15, 17 and 18, R19.

The order matters. The files of the first full source become the
library's files at each path. Dean should add the source with the
better tags first.

**S5. A folder that holds another library.** Dean adds a folder that
has its own `.medialayer` for another library.
- MediaLayer tells Dean that the folder belongs to another library. If
  that library synced in the last 30 days, MediaLayer says so, because
  another writer may still use it.
- If Dean continues, MediaLayer renames the old folder to
  `.medialayer-old-<library id>`. It never empties it. Then it handles
  the folder as in S4.
- MediaLayer does not import the other library's catalog or shared
  playlists.
- Rules 4 and 6.

**S6. A folder inside the Primary.** Dean adds a folder that is the
Primary, is inside it, or holds it.
- MediaLayer refuses, and says why (sync.md, "Checks before anything is
  written").

### Adding music

**S7. Dean adds an album in MediaLayer.**
- MediaLayer uploads the files to Proton and adds them to the catalog.
- One minute later, the sync copies them to the SSD and the server from
  the Mac's copy, not from Proton.
- R2, R21.

**S8. Dean adds an album with Proton's own app.**
- At the next hourly sync, MediaLayer finds the new files on Proton.
  When they are settled, it adds them to the catalog and copies them to
  the Secondaries.
- Rules 3 and 12, R21.

**S9. A program on the server saves an album into the server's
folder.**
- At the next sync, MediaLayer finds files at new paths on the server.
  It waits until they are settled. Then it imports them to Proton and
  copies them to the SSD.
- The activity list shows: "12 files imported from Server."
- Rules 3 and 18, R22.

**S10. A copy to the server is still running, or stopped halfway.**
- The file is not settled, so MediaLayer leaves it.
- If the copy stopped and the file stays the same for 2 minutes, it is
  settled. A truncated FLAC does not decode, so MediaLayer does not
  import it (rule 18). A truncated MP3 is imported as a new file. It does
  not replace any file, so no data is lost, and Dean can delete it.
- Rules 3 and 18.

### Editing tags

**S11. Dean retags an album in MediaLayer.**
- MediaLayer writes the new tags to the files on Proton and updates the
  catalog. The old hash of each file goes into its file history.
- At the next sync, each Secondary has the old file at each path.
  MediaLayer replaces it. The old file goes to trash.
- Rule 11, R2.

**S12. Dean fixes a title on a track that is also on a compilation.**
- The compilation's track has the same audio hash and the same old
  title.
- MediaLayer asks: "Also change the title on 1 other track with this
  audio?" There is no default, because the albums are different.
- The album name, track number and cover of the compilation do not
  change, because they were different before the edit.
- R15.

**S13. Dean retags files on the server with a program in a shell.**
- At the next sync, the server has different files at library paths,
  with the same audio. MediaLayer replaces them with the library's
  files. The retagged files go to the server's trash for 30 days.
- The activity list shows: "Server: 14 files that changed outside
  MediaLayer were replaced. The changed files are in Server's trash."
- To keep new tags, Dean must make the edit in MediaLayer or on the
  Primary.
- Rule 17, R22.

**S14. A tagger on the Mac renames and retags files in the Proton
folder.**
- At the next sync, 20 paths are gone and 20 new files are on Proton.
  Each new file has the audio hash of exactly one gone file.
- MediaLayer treats each pair as a move and a retag. It keeps the file
  ids, so playlists and ratings stay. It moves and replaces the copies
  on the Secondaries. The library does not get duplicates.
- Rule 8.

**S15. Files on Proton change outside MediaLayer, and their audio
changes.** A program rewrites 40 files.
- MediaLayer counts 40 files with changed audio in one day. This is
  more than 30, so it pauses before it changes the Secondaries.
- It shows the 40 files. Dean picks "Accept the changes" or "Restore
  the old files".
- Rule 11, Part 3 "Pauses".

### Deleting and losing files

**S16. Dean deletes an album in MediaLayer while the SSD is
disconnected.**
- MediaLayer moves the files to the trash on Proton. It writes them in
  the deletions log. It moves the server's copies to the server's trash.
- The SSD shows as offline.
- A month later, Dean connects the SSD. MediaLayer moves each deleted
  file to the SSD's trash, because its hash is in the deleted file's
  history.
- Rules 2, 7 and 16.

**S17. Dean deletes one of two identical files.** Proton has the same
bytes at `A/x.flac` and `B/x.flac`. Dean deletes `B/x.flac` in
MediaLayer.
- MediaLayer moves `B/x.flac` to the trash on Proton, and writes the
  deletions log.
- On each Secondary, `A/x.flac` matches rule 13 and stays.
- On each Secondary, `B/x.flac` has the current hash of library file
  `A/x.flac`, and the Secondary already has `A/x.flac`. So rule 15
  moves `B/x.flac` to trash. Rule 13 comes before rule 16, so the keeper is
  never at risk.
- Rules 13 and 15.

**S18. Dean deletes a file in Proton's web app.**
- At the next sync, the file is gone from Proton, and the deletions log
  does not name it.
- MediaLayer finds the file on the SSD with the same hash. It copies the
  file back to Proton.
- The activity list shows: "1 file restored to Proton from SSD."
- To delete a file, Dean must delete it in MediaLayer.
- Rule 9.

**S19. Dean reorganizes folders in Proton's app.**
- At the next sync, the old paths are gone, and the same hashes are at
  new paths.
- MediaLayer sees moves. It keeps each file id, and moves the files on
  each Secondary. Nothing crosses the network.
- Rule 8.

**S20. The Proton folder is emptied by mistake.**
- The listing shows 0 files. The catalog has 16,800.
- MediaLayer stops the sync. It shows: "Proton has far fewer files than
  the library. Nothing was changed." It offers "Restore the library to
  Proton from SSD".
- MediaLayer does not start the restore on its own, because the cause
  can be a wrong folder or a bad listing.
- Rule 1.

**S21. Proton has an outage, and its listing stops halfway.**
- MediaLayer stops the sync and changes nothing. The next sync tries
  again.
- Rule 1.

**S22. A file is lost from Proton, and the only copy is on the SSD,
which is disconnected.**
- MediaLayer looks on the server and in each trash. It does not find
  the file. The track becomes a missing track. Its playlists and
  ratings stay.
- A week later, Dean connects the SSD. MediaLayer finds the hash and
  restores the file to Proton, with the same file id.
- Rules 9 and 14, Part 3 "Missing tracks".

**S23. A file is lost from Proton, and the server has a retagged copy at
the same path.**
- The server's copy is not in the track's file history. MediaLayer
  does not replace it or remove it (rule 14).
- MediaLayer offers it to Dean: "Server has a file with this track's
  audio at the same path. Restore it?"
- Rule 14.

**S24. The only copy of a missing track is in the SSD's trash, and its
30 days end.**
- MediaLayer does not empty it from the trash, because a missing track
  needs it. When Dean connects the SSD, MediaLayer restores it.
- Rule 4, Part 3 "Trash".

**S25. A missing track has a duplicate in the library.**
- MediaLayer shows: "This track is missing. Another file has the same
  audio. Relink the missing track to it?" Dean decides.
- "Missing tracks".

**S26. Dean picks a keeper.** Version A and version B are duplicates.
Dean keeps A and deletes B. Later, an old folder on the server has a
third copy with B's exact bytes, at a new path.
- The third copy's hash is in B's history, and no library file has that
  hash. MediaLayer moves it to the server's trash, with the reason
  "Deleted in MediaLayer".
- If the third copy had other bytes, MediaLayer would import it as a
  duplicate of A (rule 18), and show "Pick a keeper" again.
- Rules 16 and 18.

**S27. Dean moves a deleted file back into the server's folder by
hand.**
- Its hash is in the history of a deleted file, so MediaLayer moves it
  to trash again. It shows: "This file was deleted in MediaLayer on
  2026-12-01. To get it back, use Undo Delete."
- Undo Delete copies the file from a trash back to Proton, and takes it
  off the deletions log.
- If Dean puts the file on Proton instead, it is a new library file
  (rule 12), and MediaLayer takes it off the deletions log.
- Rule 16.

**S28. The SSD comes back after two retags and a move.** While the SSD
was offline, Dean retagged a track twice and moved it in MediaLayer.
- The SSD's file is at the old path, with the oldest hash. That hash is
  in the track's file history.
- MediaLayer moves the file to the new library path and replaces it
  with the current file. It does not import it, and the library does
  not get a duplicate.
- Rule 15.

### Damage and attacks

**S29. Ransomware encrypts and renames the server's music.** Each file
becomes `<name>.flac.locked`.
- The library paths are now empty on the server. MediaLayer copies the
  library's files back to them.
- The `.locked` files are at new paths, and are not media that
  MediaLayer can read. MediaLayer leaves them and lists them as "not
  imported" (rule 18). It does not import them.
- If the ransomware keeps the names, the files at library paths do not
  decode or have other content. MediaLayer replaces them (rule 17). The
  encrypted files go to trash.
- Rules 17 and 18.

**S30. A program writes 2,000 new music files to the server in one
day.**
- MediaLayer pauses the imports from the server at 100 files, and asks
  Dean.
- Rule 18, Part 3 "Pauses".

**S31. A FLAC file on Proton is damaged.**
- The file changed and does not decode. MediaLayer moves it to Proton's
  trash and restores the last good file from the SSD.
- Rule 10.

### More than one writer

**S32. Dean's second Mac.** Dean installs MediaLayer on a second Mac and
signs in to Proton.
- The second Mac opens the library from Proton and is a writer. It
  reads the list of sources from `repo.json` and asks for each sign-in.
- If Dean does not sign in to the server on the second Mac, the second
  Mac skips the server. The first Mac syncs it. On the second Mac, the
  server shows "not reachable from this Mac".
- R6, R8.

**S33. Both Macs make changes at the same time.**
- The Mac that holds the sync lock writes. The other Mac waits. Then it
  takes the newest snapshot before it writes.
- When the second Mac's tag edit is for a file that the first Mac
  changed in the meantime, the second Mac shows both sets of tags
  and asks again.
- Rule 5.

**S34. One Mac does a long import while the other Mac edits a tag.**
- The import releases the lock every 10 minutes. The tag edit goes in
  then.
- R11.

**S35. Dean makes the SSD the Primary while the second Mac is off.**
- MediaLayer syncs first. Then it makes the SSD the Primary, and writes
  the new `repo.json` to the SSD and to Proton.
- When the second Mac starts, it reads Proton's `repo.json`, sees the
  move, and follows it. It has no access to the SSD (a disk on the first
  Mac), so it becomes a reader. MediaLayer warned Dean about this before
  the move.
- R3.

**S36. Dean's Mac is lost.**
- Each source holds the full library and catalog.
- Dean installs MediaLayer on a new Mac, opens the library from any
  source, and signs in to Proton. The new Mac is a writer.
- His playlists, ratings and play counts were in the lost Mac's
  personal database. They are lost, except the playlists that he
  shared. Personal database sync, a later milestone, removes this risk.
- R6, R12, R13.

**S37. Proton is not reachable.**
- Dean's Mac plays from the SSD or the server. It does not edit, import
  or delete. Each source shows its state.
- When Proton is reachable again, MediaLayer syncs.
- R10.

### Readers

**S38. Dean's daughter opens the shared copy.** Dean adds a Google Drive
folder as a Secondary and shares it view-only with his daughter. She
opens it in MediaLayer on her iPhone.
- She cannot write to the Primary, so her installation is a reader.
- She browses, plays and keeps albums offline. She makes playlists and
  rates tracks, and her play counts go up. All of this is in her
  personal database on her phone.
- She sees Dean's shared playlists, read-only. She does not see his
  ratings or play counts.
- Nothing she does writes to the Drive folder.
- Rule 5, R9.

**S39. Dean deletes a track that is in his daughter's playlist.**
- Her next snapshot does not have the track. Her playlist keeps the
  item and shows it as not available.
- If Dean uses Undo Delete, her playlist finds the track again by its
  file id.
- R16, R13.

**S40. Dean retags a track that his daughter rated.**
- The file id stays the same, so her rating stays on the track. Her
  reference gets the new hash and path.
- R16.

**S41. Dean's daughter has edit access to the Drive folder by
mistake.**
- Her installation is still a reader, because she cannot write to the
  Primary. MediaLayer refuses every write.
- If she adds a file with Google's web site, the file is at a new path
  on a Secondary. "Import new files" is off for a shared Secondary
  (question 1 in Part 6), so MediaLayer lists it as "not imported".
- If Dean tries to make the Drive folder the Primary, MediaLayer warns
  him that other people can write to it.
- Rules 5 and 18, R3.

**S42. Dean's sync is copying to the Drive folder while his daughter
opens it.**
- Her installation sees that the folder's `member.json` says copying.
  It keeps her current snapshot and tries again later.
- Rule 5.

**S43. Dean stops sharing the folder.**
- Her installation cannot open the library. It keeps her personal
  database, and shows that the library is not available.
- R13.

**S44. Dean's daughter opens two libraries.**
- Her personal database keeps the library id in each reference, so the
  playlists of each library stay apart.
- R16.

### The helper

**S45. The helper is stopped, or its inventory is old.**
- MediaLayer uses only the rows that match its own listing. For other
  files, it reads the server over the network. The sync is slower.
  Nothing else changes.
- R17, R20.

**S46. The helper has a different parser version.**
- MediaLayer does not use its tags and art. It reads the headers
  itself. It still uses the hashes if the hash version matches.
- R20.

### Names and sources

**S47. Names that differ only by case.**
- Case 1: the server is the Primary, and it is case-sensitive. It has
  `Track.flac` and `track.flac`. The SSD ignores case and cannot hold
  both. MediaLayer asks Dean to rename one of them on the server. Until
  he does, the SSD shows "incomplete".
- Case 2: Proton is the Primary, and it ignores case. The library has
  `Track.flac`. The server is case-sensitive, and has `Track.flac` and
  `track.flac`. The server's `Track.flac` is at the library path. Its
  `track.flac` is at a new path. Rules 13 to 18 handle it like any other file, and
  an import gets a suffix.
- Part 3, "Matching names".

**S48. Proton has the same bytes at two paths.**
- Both are library files (rule 12). MediaLayer shows them together. It does
  not remove either one. Dean can pick a keeper.
- R14.

**S49. Dean removes the server as a source.**
- MediaLayer stops syncing the server, and takes it off the list of
  sources. It does not remove any file from the server.
- If Dean adds the server again later, MediaLayer handles it as a new
  source (S4).
- R5.

**S50. The Primary's `.medialayer` folder is deleted.**
- MediaLayer stops the sync (rule 1). It offers to rebuild `.medialayer` on
  the Primary from the newest snapshot on a Secondary.
- Rule 1.

**S51. Proton closes Dean's account, and the Primary is lost.**
- Each sync stops, because MediaLayer cannot list the Primary (rule 1).
- Dean chooses "Replace the lost Primary" and picks the SSD, which is
  complete. MediaLayer raises the generation and writes the new
  `repo.json` to the SSD, the server and the Drive folder.
- Dean's second Mac cannot reach Proton. It finds the newer generation
  on the server, and asks Dean before it follows.
- R4.

**S52. Make Primary, and the old Primary cannot take the record.** Dean
makes the server the Primary, but Proton refuses the write of the new
`repo.json`, because Proton is full.
- MediaLayer undoes the switch. Proton stays the Primary. MediaLayer
  tells Dean to free space on Proton and try again.
- So two writers never use two different primaries.
- R3.

**S53. A FLAC file on Proton is damaged, and no Secondary has a good
copy.**
- MediaLayer leaves the file on Proton, and marks the track as damaged.
  It does not move the only copy to trash.
- Rule 10.

**S54. Dean's daughter's copy of the Drive folder has a changed cover
image.** Someone with edit access replaced `cover.jpg` in an album
folder.
- The cover is at a library path with other content. MediaLayer
  replaces it with the library's cover. The changed cover goes to
  trash.
- Rule 17.

## Part 8. Open questions

1. **Imports from a shared Secondary.** I recommend: "Import new files"
   is off by default for a Secondary that is shared with readers, and on
   for the others. A writer can change it for each Secondary.
2. **"Far fewer files".** I recommend: MediaLayer stops when the
   Primary's listing has 10% fewer files than the catalog, or 500 fewer,
   whichever is smaller. For a small library, the writer can choose
   "This is correct, continue", which handles each gone file by rule 9.
3. **Edits while the Primary is not reachable.** I recommend: refuse
   them (R10). The alternative is a queue of edits that MediaLayer
   writes later. A queue needs conflict handling.
4. **Shared playlists.** I recommend: any writer can edit a shared
   playlist. When a writer shares a playlist, MediaLayer copies it into
   the library. The copy in the writer's personal database stays.
5. **Import limit.** I recommend 100 files a day per Secondary, after
   its first sync.
6. **Trash and storage quotas.** Trash uses space on each source for 30
   days. I recommend: show the size of each trash, and offer "Empty
   trash now" to a writer. A full source stops its sync with a message.
7. **What readers can see.** Readers can read the list of sources, the
   deletions log and the names of the writers' devices. I recommend: accept
   this. To hide it, the catalog for a shared Secondary would need to
   be a reduced copy.
8. **The order of the principles.** "No surprises" is last, below
   "every copy is complete", because MediaLayer undoes changes made on
   a Secondary even when that surprises the user. I recommend this
   order. The other choice is to put "no surprises" second, and to ask
   before MediaLayer undoes a change on a Secondary.
