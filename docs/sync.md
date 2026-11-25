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

## Promoting a secondary

Promotion makes a secondary the library's primary. The old primary becomes
a secondary. The core (`internal/core/promote.go`) runs these steps in
order. In the text, "the target" is the secondary that becomes the
primary, and "the index" is the device's SQLite database of the library.

### Start

1. You right-click the target's chip and choose Make Primary. The menu
   item is available only when the chip shows synced and no sync runs.
2. The app asks you to confirm. When you confirm, it calls `Promote` in
   the core.
3. The core refuses if a sync or another job runs, or if the target is not
   one of this device's secondaries. Otherwise it starts a background job.
   While the job runs, Update Library does nothing, and the core refuses
   to add or remove a source, approve changes, or change the music
   folder.

### Sync before the switch

4. The job runs a full sync, the same sync that Update Library runs:
   1. It reads the primary's `repo.json`. If that names a different
      primary than this device's, the sync stops (see "Other devices").
   2. It indexes the primary. It lists every file and reads the headers of
      new and changed files. If the index changed, it saves a new index
      version as a local snapshot.
   3. It publishes that snapshot and any new art packs to the primary. If
      the primary's cloud is full, it shows a notice and continues.
   4. It syncs each secondary, the target included, as "Applying
      changes" describes: copies, the trash, the art packs, the snapshot
      of the current index version, `repo.json`, and last `member.json`
      with state `synced`, that version, and the current generation.
   So the target now holds every file the primary holds, the current
   index, and nothing more (except the paths listed under Known gaps). Files added to the primary since the last
   sync are copied. Files removed or edited on the target are put back.
5. The job checks the target's sync result from step 4. If the sync
   failed, or paused at the 5% limit, the job stops with the reason. The
   primary stays the primary. The sync in step 4 has still run.

### Check the target

6. The job reads the primary's `repo.json` again.
7. It reads the target's `member.json` and checks:
   - `library_id` is this library's,
   - `state` is `synced`,
   - `version` is this device's index version,
   - `generation` is the current generation.
8. It reads the target's `repo.json` and checks that it exists and names
   no newer generation.
9. If a check fails, the job stops with the reason. The primary stays the
   primary.

### Switch

10. The job makes a new `repo.json` record from the primary's:
    - `generation` is one higher,
    - `primary` is the target,
    - `members` are the target, then the other secondaries, then the old
      primary,
    - `changed_at` and `changed_by` are the time and this device.
11. It writes the new `repo.json` to the target. If this write fails, the
    job stops. Nothing else has changed, and the old primary is still the
    primary.
12. It writes the new `repo.json` to the old primary. If this write fails
    (for example, the cloud is full), the job logs the error and
    continues. The target's `repo.json` has the higher generation, and
    the higher generation wins. The next sync that can write to the old
    primary gives it the new `repo.json`.
13. It changes this device's settings:
    - the primary is the target,
    - the secondaries are the other secondaries and the old primary,
    - the old primary is marked "review first" (see step 15).
    It saves the settings. It clears the cached connection to the old
    primary and the cache of looked-up files, so the next track plays
    from the target. A track already playing finishes from the old
    primary.
14. It writes the new generation to the index. It records that the new
    primary holds the current index version (`meta.primary_version`),
    because step 4 copied that version to it.

### Sync after the switch

15. The job runs a second full sync, now from the target:
    1. It indexes the target. The target holds the same files with the
       same modification times, so the index usually does not change.
    2. It syncs each secondary. For the old primary, this is its first
       sync as a secondary. The sync compares the two folders. The old
       primary is marked "review first", so if anything would be removed
       or replaced, even one file, the sync pauses and writes its
       `member.json` with state `paused`. Such a change means that
       something changed on the old primary during the switch. The chip
       then shows a pause badge, and you choose whether to apply it.
    3. If nothing needs removal or replacement, the sync writes
       `member.json` with state `copying`. It copies any missing files,
       the art packs the old primary lacks, and the current snapshot, and
       removes the older snapshot (a secondary keeps one). It writes
       `repo.json` and then `member.json` with state `synced` and the new
       generation. It empties trash older than 30 days and clears the
       "review first" mark.
16. The job ends. The target's chip is highlighted as the primary, and the
    old primary's chip shows synced.

No library file moves or is copied in the switch itself. The target
already holds every file from step 4.

### Switch back

17. When the old primary's chip shows synced, right-click it and choose
    Make Primary. The same steps run again, and the generation goes up by
    one more.

Do not use Choose Music Folder to switch back. See Known gaps.

### When something fails

| Fails at | Result |
|---|---|
| Step 3 | Nothing changes. The status bar shows why. |
| Steps 4 to 9 | The primary stays the primary. The status bar shows why. The sync in step 4 has run. |
| Step 11 (write to the target) | Nothing changes. The old primary is still the primary. |
| Step 12 (write to the old primary) | The switch continues. A later sync writes the old primary's `repo.json`. |
| Step 13 (saving settings) | The switch is in memory only. After a restart, the device uses the old primary again. If the old primary has the new `repo.json`, its sync stops with "the primary moved"; otherwise it continues as before, and the target is refused as a secondary because it names a newer generation. |
| Step 15 (sync after the switch) | The switch is done. The old primary's chip shows the error or the pause. The next sync tries again. |

A full old primary cannot take its `member.json` in step 15, so it can't
become synced, and you can't switch back until it has space.

### Other devices

- A device that still uses the old primary reads its `repo.json` at the
  start of each sync. If that names another primary, the sync stops with
  "the primary moved". The device doesn't follow a move yet. If the old
  primary couldn't take the new `repo.json` (a full cloud), such a device
  doesn't notice the move and keeps using the old primary.
- A device that picks the new primary as its library folder reads that
  folder's `member.json` on its first sync. It names the primary id from
  `repo.json`, so the device takes on that id and continues. This works
  only if the new primary is in the device's first Google account, since
  Choose Music Folder browses only that account.
- A device that picks a secondary as its library folder is refused. That
  folder's `member.json` names an id that is not the primary's.
- A sync leaves a secondary alone when its `repo.json` names another
  primary under the same generation, or when it is the primary.

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
- Other members' `repo.json` is rewritten only by their next sync.
- Choose Music Folder only browses the first Google account, and it
  resets the local index and secondaries before the folder is checked. So
  after a promotion, switch back with Make Primary, not by picking the old
  primary. That is refused as a secondary or, if the old primary couldn't
  take the new `repo.json`, opened as a second primary at the old
  generation.
- Another device doesn't follow a promotion made elsewhere: its sync
  stops with "the primary moved" (when the old primary took the new
  `repo.json`) until the library is picked again there, which works only
  if the new primary is in that device's first Google account.
- Paths the primary lists twice (or twice by case or Unicode form) are in
  the index but not on the secondary. On exFAT or FAT disks modification
  times are kept to 2 seconds, and HFS+ disks keep names decomposed, so
  promoting such a local secondary means re-indexing it, with new file ids
  for those paths.
- A device restoring the library reads the index from the primary only, so
  while the primary is full it gets the primary's older version.
- The pause review shows counts, not the files.
