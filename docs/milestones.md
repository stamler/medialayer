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
   noticing changes on the share as they happen.
5. **macOS desktop features.** Menu bar control, cache cap with offline
   albums, write-mode toggle, the optional Tauon mount, a notarized build
   with Sparkle updates.
6. **iOS from the same project.** The iOS player check (framework size, 30
   minutes of locked-screen playback, a TestFlight upload), then the
   iPhone and iPad app, which can open a library a Mac built or set one up
   on its own.
7. **Tag editing with MusicBrainz (desktop, write mode).** Look up a
   release by its tags (or track lengths for a ripped CD), show current and
   proposed tags side by side with the Cover Art Archive cover, and write
   only after the user approves. MediaLayer downloads the file, rewrites the
   FLAC metadata blocks, uploads it to the primary, updates the index and
   packs, and sync carries it to every secondary. MusicBrainz IDs use
   Picard's tag names; lookups keep to one request per second. Gate:
   retagging the test album leaves `audio_sha256` unchanged and every
   secondary matches the primary.
8. **Merging a library on another source.** Add a folder on a different
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
9. **Tempo, pitch and beat matching.** Three playback controls on the
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
