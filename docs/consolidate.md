# Bringing sources in and consolidating them

A design for milestone 9, made general: music often lives in several
places (a server, a disk, a cloud, an old Mac folder), roughly the same
in each but not exactly. MediaLayer should take any of them in, show
what each has that the library doesn't, merge that into the library on
the user's say-so, and from then on keep the places as copies, one way,
by the rules in [sync.md](sync.md). Nothing here is built yet.

What it doesn't do: sync two ways. After consolidating, the primary is
the one place music is added; everything else follows it. Two-way sync
(conflicts, deletes that travel both ways) is a much larger and riskier
thing, and consolidating once covers the common case: music scattered
over years, not music added in two places every week.

The first case, Dean's: an SMB server over Tailscale (2,000 km away,
about 1.5 MB/s out of it) and an external disk that is often unplugged,
each holding about 500 GB of roughly the same music, and an empty Proton
Drive folder. The server becomes the primary, the disk and Proton its
copies.

## Today

Add Source refuses a folder that holds anything but this library, since
sync would overwrite it ("Checks before anything is written" in
sync.md). So a folder of music can't be added at all, not even a
near-copy (the same albums, other modification times or tags).

## What the user sees

Add Source, on a folder that already holds music that isn't this
library, offers instead of refusing:

- **Bring its music into the library.** Compare, review, merge (below).
  Afterwards the folder can become a copy, or be left as it is.
- **Make it a copy of the library.** For a folder that is already nearly
  one: the comparison shows what would change on it, including every
  file only it has, which would go to its trash, the user approves that
  once, and it is adopted (below).
- **Cancel.** Nothing has been written anywhere.

Choose Music Folder stays as it is: the folder picked becomes the
primary, and nothing in it but `.medialayer` is ever written.

## 1. Compare, read-only

Both folders are listed (the primary's listing is the indexer's) and
every media file on the other side is put in one group:

| Group | Meaning | Default in the merge |
|---|---|---|
| Same | same path, size and content (or time) | nothing |
| Same audio, other tags | same audio hash, tags or time differ | nothing; listed |
| Same track elsewhere | same audio hash at another path | nothing; listed |
| Only there | no file with its audio on the primary | copied in |
| Different audio, same path | a real conflict | nothing; listed for the user |
| Only on the primary | | nothing (merging never removes) |

Matching is by path and size first, which settles most files from the
listings alone, then by audio hash (audiohash.go), which tells a
retagged or re-timed file from a different recording and finds a moved
one. Hashes come from where they are cheapest: the index's
`audio_sha256` and the host helper's inventory for the primary
(smb.md), the device's own `audio-hashes.json`, and for the other side
reading the files where it is fast (a disk on this Mac) or the helper's
inventory if one runs there. A source reached only over a slow link is
compared by path, size and tags, and says so; its "only there" list then
may include retagged copies.

Nothing is written to either folder for this. The result is kept on the
device until the merge or a cancel.

## 2. Review

A list by group, album by album, with counts and sizes. The user can
take an album or file out of "only there", or put a conflict's other
copy in (it is copied beside the primary's, never over it). Tag
differences are shown, not applied; applying them belongs to tag editing
(milestone 8).

## 3. Merge, add-only

The chosen files are copied into the primary at their own relative
paths. A path the primary already has (a conflict the user put in) gets
a suffix (`01 Track (2).flac`). Nothing on the primary is replaced or
removed, so the merge needs neither trash nor the daily limit. The copy
runs through the device, so its speed is the slower of the two links;
for a remote primary that is the device's upload. The next sync indexes
the new files and copies them on to every copy.

This is the first time MediaLayer writes music to the primary (sync.md
says it writes there only the lock and the library's own files), so the
merge keeps its own rules:

- It holds the sync lock and checks the lease before each file.
- A path is free only if no file has it by the primary's own key (case
  and Unicode form folded where the primary is case-blind, as SMB and
  local disks usually are), checked again with `NewObject` just before
  the write; rclone's `Copy` would otherwise replace a file that
  appeared since the listing. That leaves a small race with someone
  writing the same name on the server at the same moment; the write
  goes to a temporary name and is moved into place only if the name is
  still free, where the backend allows.
- A cut merge can leave `*.partial` files on the primary; the merge
  removes its own on the next run (it knows their names), as sync does
  on a secondary.

A source that is another MediaLayer library (its own `.medialayer`)
merges its music the same way; its playlists, file ids and secondaries
are left for a later version (the open questions in milestones.md).

## 4. Adopting a near-copy

Once the merge is done (or for a folder that was nearly a copy from the
start), the folder becomes a secondary without copying everything again:

- Files the same in content are kept, with the secondary's time set to
  the primary's where only the time differs. Where both sides list a
  common hash (local disks, Drive's MD5) rclone's `Equal` already does
  this during sync's planning. For an SMB primary, which lists none,
  content equality must be read fresh on both sides (a SHA-256 of each
  whole file, the server's by the host helper at the time, not one
  cached by size and time, which a tag editor that keeps the time would
  fool). An audio hash plus size isn't enough: FLAC padding and ID3
  often keep the size across a retag, and a time set on a false match is
  never looked at again. Files not proven equal are replaced. Proton
  can't change a time after upload, so there this step doesn't apply.
- Everything else is the usual sync: copies, replacements, removals to
  the folder's trash, and the usual pause. The pause already approves
  one exact plan, once (sync.md, "The daily limit"), and tag-only
  changes don't count, so adoption needs no exemption: the plan the user
  approves is the comparison's list, and a changed listing asks again.
- The way in is new: `check()` refuses a folder holding files with no
  library record. Adoption writes the folder's `member.json` in state
  `copying` first, as sync does for any new secondary, after the user's
  approval; from then on it is a secondary being filled, and a cut
  adoption is resumed by the next sync. That resumed sync trashes what
  the primary lacks, which is why the approval lists those files.

## 5. A copy that is often offline

Today a missing folder counts as empty for any secondary, even one
fully synced before: it passes the checks and gets a full copy. On an
unplugged disk that fails at the probe (macOS doesn't let it create
`/Volumes/<disk>`) and shows as an error; a different disk mounted
under the same name would be filled instead; a cloud folder deleted by
hand would be filled again from scratch. The fix, for every kind of
secondary, comes before anything else here:

- The settings remember that a secondary has been written to. Such a
  folder that is missing, or has no `member.json`, is "offline" (a disk)
  or refused with a message (a cloud), never empty.
- A folder under `/Volumes/<disk>` is only used while that is a mount
  point (its device differs from `/`'s).
- An offline disk is skipped and shown as offline, not as an error.
  Syncing as soon as it is plugged in needs the app to watch for mounts
  (NSWorkspace's mount notification), and is then a sync like any other.

## 6. Filling a copy from another copy

A new secondary is filled from the primary through the device, so for a
remote primary every byte crosses the slow link. Another secondary can
supply the bytes instead, file by file:

- Every decision still comes from the primary's listing. A file is
  taken from the other copy only when that copy has it with the
  primary's size and time, by the test sync already uses to call two
  files the same (that copy's own time window, case and Unicode keys,
  no duplicate names); anything else comes from the primary.
- In the same run, under the same lock, the source copy is synced and
  checked first.
- The copy is given the primary's time, not the source's (an exFAT
  disk keeps 2 seconds; the source object is wrapped to report the
  primary's). Otherwise a copy on Proton, which can't change a time
  later, would differ from an SMB primary on every file, with no common
  hash to settle it.
- Proton can't be a source, as its times are its uploads'.

For Dean: once the disk is adopted, Proton is filled from the disk at
home, at the home upload speed, instead of from the server at 1.5 MB/s
(about four days for 500 GB). The other way is the server uploading
itself, the headless device in smb.md, which needs no Mac awake.

## Dean's setup, in order

1. Run the host helper on the server (smb.md), so the server's hashes are
   there.
2. Choose the server's music folder as the primary.
3. Add the disk: "Bring its music into the library". The disk is read on
   the Mac, the server from the helper's inventory, so nothing crosses
   the link but listings. Review; copy the disk-only files up.
4. Adopt the disk as a copy; it then syncs whenever it is plugged in.
5. Add Proton; it fills from the disk while that is plugged in and
   synced, otherwise from the server.

## Steps

1. A missing folder never counts as empty for a secondary once written
   to; offline disks (section 5). A small change of its own, first.
2. The comparison as a read-only command first (like `smbtime`), then
   in Add Source, with the report.
3. The add-only merge.
4. Adopting a near-copy through the usual pause; fresh content hashes for
   an SMB primary.
5. Filling a copy from a matching copy.
6. Merging another MediaLayer library's playlists and ids.

## Open choices

1. **Where merged files go.** I'd keep their own relative paths, so a
   library organised the same way stays one tree; the other choice is a
   folder per source (`Imported/<disk name>/...`), which never mixes but
   leaves the user to tidy up.
2. **A source compared only by path, size and tags.** I'd allow it, with
   the report saying so; the alternative is refusing slow sources until
   a helper runs there.
