# Syncing secondaries

How the core copies the library from the primary to each secondary, as
built in `internal/core/mirror.go`. The rules come from the architecture
proposal (see [milestones.md](milestones.md)): the primary is the truth and
changes flow one way, primary to secondaries.

## When it runs

Every sync of the library (at app start, on Update Library, and right
after a secondary is added) first brings the primary's index up to date
and publishes it, then syncs each secondary in turn. Only one sync runs at
a time on a device. A secondary that fails or pauses doesn't stop the
others; its chip in the status bar shows its state.

## Every copy is a whole library

The goal is that if the primary is lost or corrupted, any secondary can be
promoted in its place. So each secondary holds a complete, self-consistent
library: every file, plus the index and the art packs it refers to. The
index is the one made at the start of the sync, so it matches the files as
of that sync; anything changed on the primary during the sync, and the
cases under Known gaps, are reconciled by indexing (by size and
modification time) when a secondary is opened or promoted.

The index has a **version**, raised each time it changes and stored inside
it (`meta.version`, with the generation it was made under). Each version is
kept on the device as a snapshot file, and that same file is written to
the primary and to every secondary as `.medialayer/snapshots/NNNNNN.sqlite`
(NNNNNN is the version). Secondaries get it from the device, not from the
primary, so they have the newest index even when the primary couldn't take
it. A secondary keeps only the newest snapshot; the primary keeps two.
Snapshots are compared by size and MD5, never by name alone, and a version
waiting for the primary that another device's index took meanwhile is
renumbered, so one version number always names one index.

## Checks before anything is written

A secondary is synced only if it is safe to write:

- It is empty, or a copy of this library (its `repo.json` or `member.json`
  names the same library id). A folder holding anything else is refused.
- Its `repo.json` does not name a newer generation (a newer primary).
- It is not the primary, inside it, or holding it, however it was reached
  (another sign-in to the same account, a shortcut, a local path). A probe
  file written to the secondary's `.medialayer/probe/` must not be readable
  at the same path in the primary, nor appear anywhere in the primary's
  listing; a folder holding a `.medialayer` below its top is refused.
- Local folders: not the top of a disk, the home folder, or a folder a
  cloud app syncs (`~/Library/CloudStorage`, `/Volumes/GoogleDrive*`).

Nothing is written to the primary for these checks, so a full primary
doesn't block them.

## Deciding what to change

Both folders are listed in full (everything except the top `.medialayer`
and operating-system files such as `.DS_Store`). A file is the same on
both sides when its size and modification time match, as the listings
report them; copies keep the primary's modification time. Only when the
times differ are checksums compared, and the provider's checksum comes
from the listing too (Drive's MD5). Nothing is downloaded to compare. A
local secondary with a differing time is read from its own disk.

Paths are matched in Unicode NFC, and on local disks also ignoring case,
so names an HFS+ disk keeps decomposed, or a case-only rename, don't
churn.

The result is a list of copies (missing on the secondary), replacements
(different on the secondary, because the primary changed or the file was
edited on the secondary directly) and removals (not on the primary, plus
extra Drive duplicates of one name).

## The 5% pause

If replacements and removals together exceed 5% of the library's files,
the sync of that secondary pauses before changing anything. Its chip shows
a pause badge; clicking it shows the counts and asks to apply. An approval
covers exactly the changes it showed, for one sync. New copies don't
count.

## Applying changes, and the write order

1. `.medialayer/member.json` is written with state `copying`: which library
   this folder is a copy of, and that a sync is under way.
2. Removals and replacements: the secondary's file is moved to
   `.medialayer/trash/<time>/<path>` first. Trash folders are emptied after
   30 days.
3. Copies and the replacements' new versions, one file at a time.
4. The library's own files: the art packs the secondary lacks, then the
   index's current version (older snapshots are removed), then `repo.json`
   last.
5. `member.json` is rewritten with state `synced`, the index version and
   snapshot it now holds and matches, the generation, and the time.

A sync that pauses sets `paused`, keeping the snapshot and time it last
matched. A sync that fails before changing anything leaves `member.json`
as it was, so `synced` always means "matched this snapshot at this time".

## Each file is all or nothing

A Drive upload creates the file only when all of it has arrived, so an
interrupted copy leaves no partial file. rclone checks the size and
checksum after each transfer and removes a copy that doesn't match. On a
local disk, rclone writes to a temporary name and renames it when done.

## Resuming

Quitting the app, losing the network or a failed transfer just ends the
sync. The next one (the app starts one when it opens) lists both sides
again; files already copied match and are skipped, so it carries on with
the rest. A folder cut short before its first `repo.json` is still
recognised by its `member.json` and resumed rather than refused.

Between two Google accounts there is no server-side copy, so each file is
downloaded and uploaded through the device; within one account Drive
copies on its own servers.

## A full primary

Sync writes to the primary only to publish the library's own files (the
snapshot and art packs, a few MB when the library changed). If the
primary's cloud is full, that upload fails: the status bar says so, the
device keeps offering that version to the primary on every sync
(`meta.primary_version` records the newest the primary holds), and the
secondaries still get every file and the current index version.

## Is a secondary safe to promote?

Read its `.medialayer/member.json`. It is a complete, current copy when:

- `state` is `synced` (not `copying` or `paused`),
- `library_id` is the library's, and `generation` is the current one
  (the highest in any member's `repo.json`),
- `version` is the newest index version known: the highest of the
  primary's newest snapshot, the device's own index, and the other
  members' `member.json`.

A `synced` secondary with an older `version` is a whole, consistent library
as of that version (its files and index match each other at `synced_at`);
it is only missing later changes.

## Promoting a secondary (planned)

Not built yet. Promotion will: check the secondary as above; raise the
generation by one and write a `repo.json` naming the secondary as primary
(with the time and device) to the new primary first, then to every other
member it can reach; and treat the old primary as a secondary from then
on. Because the highest generation wins, a device that still names the old
primary switches when it next reads any member. Changes found later on the
old primary are kept in its trash and flagged, never synced over the new
primary.

## Known gaps

- A replacement moves the secondary's old version to trash before copying
  the new one. If the sync stops in between, that file is missing from the
  secondary until the next sync. It is still on the primary and in the
  secondary's trash.
- No sync lease yet: two devices syncing the same library at once is not
  guarded against.
- The 5% limit is counted per sync, not per day.
- Each device lists its own secondaries in the primary's `repo.json`, so
  two devices with different secondaries overwrite each other's list.
- Promoting a secondary to primary is designed (above) but not built.
- Paths the primary lists twice (or twice by case or Unicode form) are in
  the index but not on the secondary. On exFAT or FAT disks modification
  times are kept to 2 seconds, and HFS+ disks keep names decomposed, so
  promoting such a local secondary means re-indexing it, with new file ids
  for those paths.
- A device restoring the library reads the index from the primary only, so
  while the primary is full it gets the primary's older version.
- The pause review shows counts, not the files.
