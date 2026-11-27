# Milestones

The built-in player comes first, and the macOS app comes first among the
players. The full reasoning is in the architecture proposal:
https://claude.ai/artifact/4F35w22KCPz88bmpWkrhjK

## Shape of the apps

- One Go core, built on rclone's backends, compiled for every platform.
- macOS, iPhone and iPad share one multiplatform SwiftUI project. A shared
  Swift package (MediaLayerKit) reads the SQLite index with GRDB, plays
  through AVFoundation, and bridges to the Go core. gomobile builds the
  core into one xcframework with macOS, iOS and simulator slices.
- macOS only: Consolidate, write mode and full-file audio hashing, a menu
  bar control, the 10% cache cap with offline albums, the optional
  mount for Tauon and the localhost Subsonic endpoint. It ships as a
  notarized download with Sparkle updates, because the Mac App Store
  sandbox does not allow the mount or the local server.
- Later: Android in Kotlin with Jetpack Compose, using a gomobile AAR and
  Media3. Windows and Linux are decided when we get there, leaning toward
  one shared Go desktop app (such as Wails) with libmpv.

## Setup

Open the app, connect one or more clouds, pick the music folder, and choose
the primary. No command line.

- Google Drive signs in through MediaLayer's own OAuth client (users never
  create client IDs). Proton signs in with the account's password (and
  2FA code), through rclone's backend, keeping only the session; SMB uses
  a username and password; iCloud Drive on Apple devices needs nothing.
- If the folder already has a `.medialayer` repository, the library appears
  in seconds. Otherwise indexing starts automatically, on any device,
  iPhone included, and resumes if interrupted.
- The primary is the truth. Changes flow one way, primary to secondaries.
  Deletes go to a `.medialayer/trash` folder on secondaries for 30 days; a
  mass change (over 5% of files or 30 files in a day, whichever is
  smaller, not counting tag edits) pauses sync; a file changed on a
  secondary outside MediaLayer is moved to trash and replaced from the
  primary. One device syncs at a time, holding a lease in the primary's
  `.medialayer`.
- Every cloud's `.medialayer` holds the library's metadata and database
  plus a primary record: library id, which cloud is primary, a generation
  number raised on every change of primary (with time and device), and the
  member clouds. The highest generation wins, so clock skew can't pick the
  wrong primary. Connecting a secondary first shows where the primary is
  and offers "connect the primary" (recommended) or "make this the
  primary"; meanwhile the library plays read-only from the secondary. If
  two offline devices promote different clouds to the same generation,
  the later time then the higher device id wins everywhere, and the user is
  warned. A replaced primary's late changes are kept in trash and flagged,
  never synced over the new primary.

## Feel

The app must feel buttery smooth: scrolling, resizing the window, typing
in search, and showing or hiding the sidebar stay at the display's frame
rate with a 10,000-track library, with no dropped frames or beachballs. The
Mac player is measured against this (see [ui-framework.md](ui-framework.md))
and work on it continues after the AppKit table.

## Next

1. **Core library and connecting a cloud.** A Go package with a small
   interface: connect a cloud, index a folder, sync the index and packs,
   sync primary to secondaries, get a stream for a track. Built into the
   xcframework and a Mac test binary. Google sign-in with MediaLayer's own
   OAuth client in Google's testing mode; test whether a folder picked with
   the `drive.file` scope covers its contents.
2. **Automatic indexing.** Starts when a folder is picked, resumable,
   writes the SQLite index, header packs and art packs to `.medialayer` on
   the primary. Gate: the 407-file test library indexes without reading
   audio, and a second device opens it in seconds.
3. **macOS player, first version.** The setup flow, then a design modelled
   on the iTunes 11/12 Songs view, in light and dark mode: a toolbar with
   transport controls, a now-playing display and search; a column browser
   (Genres, Composers, Artists, Albums) above a sortable, customisable track
   table with an album art column and a Clouds column; a status bar with
   count, time and size; a collapsible sidebar for the library and
   playlists. Playback with Now Playing and media keys. The track table may
   need AppKit's `NSTableView` for the art column and for speed. Gate: a new
   user goes from opening the app to playing a track with no other steps, a
   track starts within about 2 seconds, and a 10,000-track table scrolls
   smoothly.
4. **Secondary sources.** Add a second folder (another Google Drive
   folder or account, a folder on the Mac or a disk, then Proton or SMB) as
   a secondary and sync from the primary with the rules above, starting as
   soon as it is added. The status bar shows each source with an icon for
   its cloud and highlights the primary. Gate: a secondary built from
   scratch matches the primary file for file, a mass change pauses and
   asks, and switching the primary works. The proof of concept does the
   one-way sync, trash, pause, generation check and switching the primary
   (Make Primary), holds the sync lease, and counts the daily limit
   (the smaller of 5% and 30 files over a rolling 24 hours, with tag
   edits told apart by audio hashes: decoded samples for FLAC and ALAC,
   audio bytes for MP3 and AAC, made by a background pass over every
   track). Proton Drive works as a secondary, signing in with the
   account's password and 2FA code and keeping only the session, and a
   synced Proton secondary can be made the primary; one Proton account
   is connected at a time. SMB comes next. How it works now: [sync.md](sync.md).

   An idea for SMB, to be worked out: a small helper installed on the
   machine that serves the share pre-generates the library there, all of
   it or the slow parts (the index from file headers, art packs, perhaps
   audio hashes), reading files at disk speed instead of over the
   network. Onboarding a large SMB library would then take a fraction of
   the time, and the helper might also do some rudimentary sync, such as
   noticing changes on the share as they happen. The design, with
   Tailscale and the keychain: [smb.md](smb.md).
5. **macOS desktop features.** Menu bar control, cache cap with offline
   albums, write-mode toggle, the optional Tauon mount, a notarized build
   with Sparkle updates.
6. **Sharing a library read-only.** The owner shares the library folder
   view-only with another account, such as a child's, and that person's
   copy of the app plays it: the notarized build from milestone 5 on a
   Mac, and the iOS app of milestone 7, which has this mode from its first
   version. Setup offers "Open a library shared with me", which lists the
   account's shared folders. The core opens the folder through a wrapper
   that refuses every write, so a folder shared with edit rights by
   mistake is left alone too. A reader never indexes, publishes, runs the
   hash pass (which would download the whole library) or takes or waits
   for the sync lock; it needs none of them, since the lock only guards
   writes and the owner's snapshots carry the index, art packs and
   `audio_sha256`. The reader's own playlists, and later play counts and
   ratings, go in a local overlay.
   - The reader takes the folder's newest snapshot and art packs, and
     looks for a newer snapshot at app start and on Update Library. It
     shows the library as the owner's last sync left it; files the owner
     adds appear after the owner's device syncs. A track whose file has
     gone fails to play until the next snapshot. New work: today a
     snapshot is restored only into an empty index, only inside a sync
     under the lock, and a folder that is a secondary is refused.
   - The primary is the best folder to share. It keeps two snapshots and
     uploads art packs before each snapshot, so a reader always finds a
     whole version. A shared secondary keeps one snapshot and changes its
     files during a sync, so the reader waits while its `member.json`
     says `copying` and retries if the snapshot vanishes. After Make
     Primary, a shared old primary still holds a whole library, current
     again once its first sync as a secondary succeeds; the reader
     doesn't follow `repo.json` to a primary it can't reach. A shared
     folder the owner stops syncing looks just like a library that hasn't
     changed, so telling the reader would need each sync to stamp the
     folder even when nothing changed.
   - The overlay is a SQLite database on the reader's device, per library
     id, attached to the index (SQLite ATTACH) so the app can join them.
     The owner's playlists show read-only beside the reader's. Rows name
     tracks by file id, which survives retags and most moves and renames,
     and also keep the track's path and `audio_sha256`, to find a track
     the owner indexed again under a new id once the owner's hash pass
     has hashed it. A reader on a different hash version than the owner
     would clear those hashes, so sync.md's rule (change the version only
     with a release every device takes) covers readers too. Backing the
     overlay up to the reader's own cloud, so it survives a reinstall and
     follows them to another device, comes later.
   - Google Drive is reachable through rclone: the drive backend opens a
     folder shared with the account (`shared_with_me`, or the folder's id
     as `root_folder_id`, with `resource_key` for old link-shared
     folders). The owner must not turn off downloads for viewers (Drive's
     `copyRequiresWriterPermission`, set per file). MediaLayer asks for
     the full `drive` scope, which covers shared folders; if milestone 1
     moves to `drive.file`, a shared folder works only if picking it in
     Google's picker grants its contents. In Google's testing mode each
     reader's account must be a test user, and a child's supervised
     account may need a parent's approval to sign in.
   - Proton Drive is not reachable today. rclone's Proton backend, on the
     Proton-API-Bridge library, opens only the account's own main share,
     so neither a folder shared with a Proton member nor a public link
     can be opened. That needs upstream work in rclone's backend and in
     Proton-API-Bridge. Until then, a Proton library is shared through a
     Google Drive secondary.
   - An SMB share with a read-only account, or a disk, opens the same way.
     A view-only iCloud Drive shared folder appears in the file system on
     Apple devices and should open as a local folder; untested.

   Gate: a second Google account with view-only access opens the test
   library in seconds and starts a track within about 2 seconds. It makes
   a playlist, which is still there after the owner adds a track and syncs
   and the reader takes the new snapshot and shows that track. A file set
   so viewers can't download fails with a clear message. The shared
   folder is unchanged afterwards, and in the tests the wrapper saw no
   write. Make Primary on the owner's device doesn't stop the reader
   playing.
7. **iOS from the same project.** The iOS player check (framework size, 30
   minutes of locked-screen playback, a TestFlight upload), then the
   iPhone and iPad app, which can open a library a Mac built or set one up
   on its own.
8. **Tag editing with MusicBrainz (desktop, write mode).** Look up a
   release by its tags (or track lengths for a ripped CD), show current and
   proposed tags side by side with the Cover Art Archive cover, and write
   only after the user approves. MediaLayer downloads the file, rewrites the
   FLAC metadata blocks, uploads it to the primary, updates the index and
   packs, and sync carries it to every secondary. MusicBrainz IDs use
   Picard's tag names; lookups keep to one request per second. Gate:
   retagging the test album leaves `audio_sha256` unchanged and every
   secondary matches the primary.
9. **Merging a library on another source.** Add a folder on a different
   source that already holds music, or already has its own `.medialayer`
   library, and merge the two libraries into one. Details are to be
   decided. Today such a folder is refused as a secondary, because sync
   would overwrite it (see "Checks before anything is written" in
   [sync.md](sync.md)). Open questions:
   - Which library id and primary survive, and what happens to the other
     library's primary record and generation, which may be higher.
   - How the same track in both folders is recognised (path, tags, or
     `audio_sha256` once both libraries are hashed) and which copy wins.
   - Whether files are copied into the primary's folder layout or keep
     their own paths.
   - What the added folder becomes afterwards. As a secondary, any file
     not first copied to the primary would go to trash, and the merge
     would trip the daily limit.
   - What happens to the other library's secondaries, and to devices
     holding its index, since both name a library id that no longer
     exists.
   - How playlists and other library data are combined, and whether a
     library nested below the folder's top is covered.
   - How the user previews and approves the merge before anything is
     written, as tag editing does.

   Gate: to be decided.
10. **Tempo, pitch and beat matching.** Three playback controls on the
    Mac and iOS players. Time stretching changes speed without changing
    pitch; pitch shifting changes key without changing speed, in
    semitones; beat matching brings the next track to the current track's
    tempo and lines up their beats through a crossfade. `AVPlayer` can
    stretch time but can't shift pitch on its own or play two tracks in
    sync, so playback moves to `AVAudioEngine` with `AVAudioUnitTimePitch`.
    The engine can't play the core's localhost stream as a URL, so the app
    fetches and decodes the stream itself and handles seeking, buffering
    and (on iOS) interruptions and route changes. Beat matching needs each
    track's tempo and beat grid, measured from the audio when the track
    plays (or is cached, on the Mac), with a BPM tag as a hint. These
    measurements stay in a cache on each device rather than in the shared
    index, so listening never republishes the index; sharing them is a
    later choice. Indexing still never reads audio. Gate: a track stretched
    from 0.5x to 2x keeps its pitch, a shift of plus or minus 12 semitones
    keeps its speed, two tracks from the test library mix with beats that
    stay aligned through the crossfade, a track still starts within about
    2 seconds, and the Feel targets above still hold.
