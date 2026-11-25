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
  create client IDs). Proton uses account sign-in as rclone does; SMB uses a
  username and password; iCloud Drive on Apple devices needs nothing.
- If the folder already has a `.medialayer` repository, the library appears
  in seconds. Otherwise indexing starts automatically, on any device,
  iPhone included, and resumes if interrupted.
- The primary is the truth. Changes flow one way, primary to secondaries.
  Deletes go to a `.medialayer/trash` folder on secondaries for 30 days; a
  mass change (over 5% of files in a day) pauses sync; a file changed on a
  secondary outside MediaLayer is moved to trash and replaced from the
  primary. One device syncs at a time, holding a lease in the primary's
  `.medialayer`.

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
3. **macOS player, first version.** The setup flow plus artists, albums,
   tracks and artwork from the index; playback with Now Playing and media
   keys; playlists. Gate: a new user goes from opening the app to playing a
   track with no other steps, and a track starts within about 2 seconds.
4. **macOS desktop features.** Menu bar control, cache cap with offline
   albums, write-mode toggle, the optional Tauon mount, a notarized build
   with Sparkle updates.
5. **iOS from the same project.** The iOS player check (framework size, 30
   minutes of locked-screen playback, a TestFlight upload), then the
   iPhone and iPad app, which can open a library a Mac built or set one up
   on its own.
6. **Secondary clouds.** Add SMB or Proton as a secondary and sync from the
   primary with the rules above. Gate: a secondary built from scratch
   matches the primary file for file, and switching the primary works.
