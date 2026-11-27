# Syncing secondaries

How the core copies the library from the primary to each secondary, as
built in `internal/core/mirror.go`. The rules come from the architecture
proposal (see [milestones.md](milestones.md)): the primary is the truth and
changes flow one way, primary to secondaries.

## When it runs

Every sync of the library (at app start, on Update Library, and right
after a secondary is added) first brings the primary's index up to date
and publishes it, then syncs each secondary in turn. Only one sync runs at
a time on a device, and only one device syncs a library at a time (see
"The sync lock"). A secondary that fails or pauses doesn't stop the
others; its chip in the status bar shows its state.

## The sync lock

A device holds the library's sync lock (a lease) for the whole of a sync:
indexing, publishing and copying to the secondaries. It is built in
`internal/core/lease.go`.

- **Where it lives.** At the top of the primary's `.medialayer` folder,
  one small file per device, `lease-<device id>.json`, with the device's
  name, a state (`waiting`, `held` or `released`), a token for this take
  of the lock, a counter raised on every write, and when it was taken and
  last renewed, for example:

  ```json
  {
    "device": "01M8C6FWHCDCD4R944YMM6E3K5",
    "name": "Studio Mac",
    "state": "held",
    "token": "01M8C6G2Y4R6TQK0B9J7ZP3D1X",
    "seq": 7,
    "taken": "2026-11-26T15:02:10Z",
    "renewed": "2026-11-26T15:14:10Z"
  }
  ```

  A claim released by another device that took over also has
  `taken_by`, that device's id. Each device writes only its own file,
  because Drive has no "create only if absent": two devices writing one
  shared file at once would leave two files of the same name. The files are not in a folder
  of their own, as two devices creating that folder at once could leave
  two folders of one name on Drive.
- **Taking it.** The device first checks that the primary is still its
  primary (nothing is written otherwise), then lists `.medialayer`. If
  another device's claim is live, it doesn't write and waits. Otherwise
  it writes its claim as `waiting` and lists again. If no other live claim
  is there, it rewrites its claim as `held` and syncs. If it finds
  another, it marks its own `released` and backs off; two devices that
  start together both back off and retry after a random 1 to 5 seconds,
  three tries in all. After taking the lock it checks the primary again,
  as another device may have switched it meanwhile. A claim that can't be
  read counts as held, as it may be a file caught mid-write.
- **Renewing it.** Every 2 minutes. The renewal reads the device's own
  claim first: if another device marked it released, the lock was taken
  over and the sync stops. If the device finds another `held` claim, it
  stops; the other does the same at its next renewal. `waiting` claims
  are ignored, as their writers back off. A failed renewal is tried again
  every 15 seconds. A device that couldn't renew for 9 minutes (a Mac
  asleep, say, or a renewal stuck on the network) stops before anyone
  else could take over: every write of the sync (each file copied,
  trashed, uploaded or removed, including old snapshots and expired
  trash, `member.json` and `repo.json`) first checks that
  the lock was renewed within 9 minutes, by both the wall clock and the
  device's own uptime clock, and a stuck renewal gives up at that time. Each 15-second check also
  cancels the sync on its own once the lock has lapsed, even while a
  renewal is still waiting on the network.
  A device that lost the lock tries again about 2 minutes later, and the
  sync resumes as described under Resuming.
- **Releasing it.** When the sync ends, the claim is rewritten as
  `released`, also when the lock was lost. It is rewritten rather than
  deleted, so Drive's trash doesn't fill with lock files. A crash leaves
  it held until it goes stale.
- **A device that dies holding it.** A claim not renewed for 10 minutes
  is stale. Another device judges that only by watching the file stay
  unchanged for 10 minutes on its own clock, never by the time written in
  it, so a wrong clock on either device can't free the lock early. The
  watch is kept in memory, so after the app restarts it starts again.
  The device taking over marks the stale claim `released`, naming itself,
  and the status bar shows a notice naming the device that stopped.
- **A full primary.** If the claim can't be written because the
  primary's cloud is full, the sync stops with an error saying so, and
  nothing is synced until there is space. A renewal refused for a full
  cloud stops the sync too. This also blocks Make Primary away from a
  full primary.
- **What you see.** Playback and browsing are never affected.
  - While another device syncs (or is taking the lock), the status bar
    says "Syncing on <device>. This Mac will sync after it." The core
    tries again about every 2 minutes, and Update Library tries at once.
    The device's name is its host name, with hyphens shown as spaces.
  - After taking over a stale lock, the status bar shows a notice: "Took
    over syncing from <device>, which stopped without finishing."
  - A lost lock shows "Update failed: this device lost the sync lock: …"
    with the reason (taken over, held by another device too, or not
    renewed in time), and the core tries again about 2 minutes later.
  - A full primary shows "Update failed: the primary's cloud is full, so
    this device can't take the sync lock; free up space there to sync".
  - For the apps, the core's status reports a waiting device as job
    phase `waiting`, with `holder` naming the device syncing.
- **Make Primary** holds the old primary's lock for the whole job, and
  takes the target's lock before the switch, keeping it through the sync
  after the switch. If another device holds either, the job waits as a
  sync does, and the retry is a plain sync; choose Make Primary again
  once it is done.

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

Paths are matched in Unicode NFC, and on local disks and Proton Drive
also ignoring case, so names an HFS+ disk keeps decomposed, or a
case-only rename, don't churn.

On Proton Drive, which keeps SHA-1 and can't change a file's
modification time after its upload, a file whose time alone differs is
the same file when its size and SHA-1 match, rather than one to upload
again. The `.medialayer` files are compared by SHA-1 there too, where
Drive and local disks use MD5.

The result is a list of copies (missing on the secondary), replacements
(different on the secondary, because the primary changed or the file was
edited on the secondary directly) and removals (not on the primary, plus
extra Drive duplicates of one name).

## The daily limit

Each secondary may take a limited number of removals and replacements a
day without asking: 5% of the library's files (rounded down) or 30 files,
whichever is smaller. So 5% applies below 600 files and 30 from there up;
a 20,000-track library can't lose 1,000 files in one go.

- **Counted over a rolling day.** The last 24 hours, not a calendar day,
  so there is no midnight to time a burst around and no time zone to
  agree on. The count lives in the secondary's `member.json` as `recent`,
  one entry per sync with its time and how many changes counted, so any
  device syncing that folder sees the same count. Entries older than a day
  are dropped when the record is next written. A sync's entry is written
  with the `copying` record, before any change is made, so a sync cut
  short still counts.
- **What counts.** Removals (including extra Drive duplicates) and
  replacements. New copies don't count. A replacement that only changed
  tags doesn't count either (see "Tag edits and audio hashes" below).
- **The pause.** If this sync's counted changes plus the day's would pass
  the limit, the sync of that secondary pauses before changing anything.
  Its chip shows a pause badge; clicking it shows what this sync would
  do, how many of the replacements are only retagged, how many changes
  the day already had, and the limit. An approval covers exactly the
  changes it showed, for one sync. Approved changes were reviewed, so
  the day starts again after them: `recent` is cleared.
- **A demoted primary** still asks before any removal or replacement,
  tag edits included (see "Sync after the switch").

### Tag edits and audio hashes

A file's audio hash (`audio_sha256` in the index) is a SHA-256 of its
audio and nothing else, so retagging a file, or changing its cover,
leaves it as it was:

- **Lossless files (FLAC, ALAC) hash their decoded samples.** The same
  audio can be encoded in many ways (another compression level or block
  size, a re-encode, another encoder or lossless codec), so the encoded
  bytes can differ while the audio is identical. The hash covers `pcm`
  and a zero byte, the sample rate, channel count and bits per sample,
  then every sample, interleaved by channel in WAVE order, little-endian,
  in the fewest whole bytes that hold the bits per sample (the layout
  FLAC's own MD5 uses). So a FLAC file and an ALAC file of the same audio
  hash the same. Every sample must fit the bits per sample; a file that
  fails to decode is damaged and gets no hash.
  - **FLAC**, native or in Ogg (`internal/media/flacdec.go`, hash in
    `flacpcm.go`, Ogg in `ogg.go`): MediaLayer's own decoder, for every
    bit depth FLAC allows (4 to 32), so FLAC in Ogg hashes the same as
    the native file. Ogg pages' CRCs and sequence numbers are checked too.
    It keeps samples as 64-bit integers, which also hold the 33-bit side
    channel of 32-bit stereo. Each frame's CRC-8 and CRC-16 are checked,
    and the samples are checked against the STREAMINFO block's MD5 and
    sample count where the encoder recorded them, with no audio frame
    after the count.
  - **ALAC**, in MP4 or CAF (`internal/media/alac.go`, containers in
    `mp4.go` and `caf.go`):
    MediaLayer's own decoder, after Apple's reference decoder and
    ffmpeg's, for 4 to 32 bits and up to 8 channels, which it puts in
    WAVE order. ALAC has no checksum of its own, so a damaged file is
    caught only when it fails to decode. The packets are read in the
    order the container's packet table gives, in one pass over the audio
    after the tables (MP4's `moov` box, CAF's `pakt` chunk), which are
    read first, wherever they are.
  - Both decoders' tests check ffmpeg-encoded files
    (`internal/media/testdata`, made by `scripts/make-audio-fixtures.sh`)
    against ffmpeg's own decoding, and FLAC and ALAC copies of the same
    audio against each other. ffmpeg's ALAC encoder never uses ALAC's
    "extra bits" (which Apple's encoder uses for 24-bit audio), so that
    path is checked only against the reference decoders' code.
- **Lossy files hash their audio bytes.** Decoding them gives no truer
  audio, so the hash is of the file without its tags. For MP3, that is
  the file less any ID3v2 tags at the start and an ID3v1, APEv2 or
  Lyrics3 v2 tag at the end (`internal/media/audiospan.go`). For AAC in
  MP4, it is the decoder configuration (the object type and
  AudioSpecificConfig from `esds`, not the bit rates muxers rewrite) and
  every packet, in order, wherever they sit in the file
  (`internal/media/mp4.go`), so a remux keeps the hash.

`meta.audio_hash_version` in the index names this way of hashing (`3`).
An index opened with another version has its hashes cleared, so they are
made again. That includes an index restored from the primary, so two
devices running different versions would each clear the other's hashes:
change the version only with a release every device takes.

**The hash pass.** Indexing never reads audio. Instead, once a sync ends
well, a background pass (`internal/core/hashpass.go`) hashes every
track on the primary that has no hash yet, one file at a time, reading
each in full: the first pass downloads the library once. It stops when
the next job (a sync, Make Primary) starts, as jobs rewrite the index,
and carries on after it (a write it had begun ends before the job does
anything). Each hash goes into the index at once, and the
index is marked changed, so the next sync publishes a new version that
carries the hashes to the secondaries and other devices. A file that
can't be hashed is logged and tried again only in a later session. For
the apps, the core's status reports the pass as `audio_hashes`:
`running`, `hashed`, `total` and `failed`.

**In sync.** When a sync would pass the limit, its removals alone would
not, and some of its changes are replacements of tracks, both
copies of each (the primary's and the secondary's) are hashed, from the
device's store where known and otherwise by reading them in full. Where
the two match, the replacement is a tag edit: it is still made, but
doesn't count. A file that can't be read, decoded or parsed counts as a
real change. Once the pass has run, the secondary's copy is usually known
(it was the primary's file before), so only the primary's new file is
read.

Hashes are kept so a file is read once:

- On the device, in `audio-hashes.json` in the app's data folder, by the
  file's MD5 where the cloud lists one (Drive), so the primary's file is
  known again once it is on a secondary; on a local disk by folder, path,
  size and modification time. Keys carry the hash version. Up to 50,000
  are kept.
- In the index, for the primary's file, as `audio_sha256`. A file
  indexed again because it changed loses its hash until the pass or a
  sync makes it again. Sync doesn't read these back, as a file rewritten
  with its size and time kept would still match its row; they are for
  later features such as tag editing and merging.

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

Sync writes to the primary only for the sync lock and to publish the
library's own files (the snapshot and art packs, a few MB when the
library changed). A primary too full for the lock's small file stops the
sync (see "The sync lock"). If the lock fits but the snapshot doesn't,
that upload fails: the status bar says so, the
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
   one of this device's secondaries. Otherwise it starts a background job,
   which takes the primary's sync lock (see "The sync lock") and holds it
   to the end.
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
   failed, or paused at the daily limit, the job stops with the reason. The
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

Before the switch the job takes the target's sync lock too, and holds it
through step 15.

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
| Taking either sync lock | Nothing more changes. If another device holds it, the status bar names it and a plain sync follows later. A full primary can't take its lock, so Make Primary can't move away from it. |
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

## Proton Drive

A folder on Proton Drive can be a secondary, with the same rules as a
Google Drive one. Add Source signs in with the Proton username and
password, then a 2FA code or the mailbox password if the account uses
one. The core signs in itself (rclone's backend handles a 2FA code from
an authenticator app only, and would leave an account whose two-step
sign-in is a security key half signed in) and hands the session to
rclone's backend. An account whose two-step sign-in is a security key
signs in with the key plugged in over USB, as Proton's Bridge does: the
core reads the WebAuthn options Proton sends with the first step, has
the key sign Proton's challenge (a touch, after its PIN when the key has
one, as Bridge asks), and sends the signature back as the second step.
The app asks for the PIN on a step of its own, sent only by clicking
Continue (a YubiKey touched early types a one-time code and presses
Return), and refuses one that looks like such a code before the key or
Proton sees it; the PIN goes to the key only, and nothing keeps it. The
app then shows "Touch your security key now" on a screen with no fields
while the key waits. With an authenticator app as well, a code entered is used
instead. One key at a time; a passkey kept in iCloud Keychain (which only
Proton's own apps and website may use) can't sign in. The core identifies
itself to Proton with the app version a release of rclone sends. The password goes to Proton only: the core keeps the session Proton
returns, in its own rclone config. That session includes the salted key
password that unlocks the account's keys. The Mac app encrypts the whole
config (Google's tokens too) with rclone's config encryption, under a
random key it keeps in the login keychain, as Proton's own Drive app
keeps its session in the keychain. The file alone opens nothing. If the
key is lost, the core sets the unreadable file aside and the clouds
need connecting again.

rclone's backend clears a saved session whenever opening with it fails,
offline or cancelled as much as when Proton ended it, and then signs in
with the password, which MediaLayer doesn't keep. So the core never lets
it clear the session: a failed open says the sign-in may have ended, and
works again once Proton can be reached. If Proton really ended the
session, Add Source, "Proton Drive, sign in again" fixes it.

- **As the primary.** A synced Proton folder can be made the primary
  with Make Primary, like any other. Proton keeps files in encrypted
  4 MB blocks, so reading a file's tags, or starting or seeking a track,
  fetches and decrypts at least one block: indexing new files and
  starting tracks are slower than on Drive. Listings always ask Proton
  for a folder's files, so files added with Proton's own apps and other
  devices' sync lock claims are seen; the backend caches only keys and
  folder links, and the core opens a fresh client for each sync, so that
  cache lasts one sync. Proton keeps modification times to the second,
  so after the switch each indexed file whose time differs by less than
  a second is the same file, keeping its id, and takes Proton's time;
  nothing is read again. Each renewal of the sync lock adds a revision
  of its small claim file on Proton. Choose Music Folder still browses
  Google Drive only, so a Proton primary is reached by Make Primary.
- **One account at a time.** rclone's Proton backend keeps the config
  that a renewed session is saved to in a package variable, set by
  whichever Proton remote was opened last. With two accounts open at
  once (a sync to one while browsing the other), one account's session
  could be saved under the other's remote. Signing in to a different
  account replaces the connected one only while the library doesn't use
  it. Fixing this upstream (keeping the config per remote) would lift
  the limit.
- **Case.** Proton isn't known to tell names apart by case alone, so
  paths are matched ignoring case, as on local disks: of two primary
  paths that differ only by case, neither is kept there.
- **One session, several clients.** Each Proton folder a sync opens,
  and the folder picker, is a client of its own on the one session.
  When the access token expires, the first to renew it changes the
  refresh token, and another that tries to renew with the old one is
  refused for the rest of that sync (the session in the config stays
  good, so the next sync works). Two Proton secondaries in one account,
  or browsing Proton during a sync to it, can hit this. Sharing one
  client per account would fix it.
- Signing in to another account, or again, doesn't log the old session
  out at Proton; it ends there on its own.
- Unfinished uploads leave drafts on Proton. MediaLayer is the only
  writer of its folders, so it replaces its own drafts on the next try.

## Known gaps

- A replacement moves the secondary's old version to trash before copying
  the new one. If the sync stops in between, that file is missing from the
  secondary until the next sync. It is still on the primary and in the
  secondary's trash.
- The sync lock relies on Drive listing a file as soon as its upload
  ends. A claim left `waiting` or `held` by a device that crashed blocks
  others for 10 minutes after they first see it, or longer if they
  restart meanwhile.
- A full primary blocks syncing and Make Primary; there is no way yet to
  move the library off a full primary from the app.
- The daily limit's count is kept on the secondary, so a change made
  there outside MediaLayer to `member.json` resets it.
- Tag edits are told apart only for the formats MediaLayer reads (FLAC,
  MP3, ALAC in MP4 or CAF, AAC in MP4); other files that differ always count.
- The hash pass runs wherever the core runs, so on an iPhone it would
  download the whole library over the phone's connection; the iOS app
  needs to limit it (Wi-Fi only, say) or turn it off.
- The hash pass doesn't yet slow down while a track streams, so on a slow
  connection the first pass competes with playback.
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
- Proton Drive has been built against rclone's backend but not yet run
  against a real account by the sync tests, which use local folders.
