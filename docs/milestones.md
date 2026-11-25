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
- macOS only: library building (scan, prime, Consolidate, write mode), a
  menu bar control, the 10% cache cap with offline albums, the optional
  mount for Tauon and the localhost Subsonic endpoint. It ships as a
  notarized download with Sparkle updates, because the Mac App Store
  sandbox does not allow the mount or the local server.
- Later: Android in Kotlin with Jetpack Compose, using a gomobile AAR and
  Media3. Windows and Linux are decided when we get there, leaning toward
  one shared Go desktop app (such as Wails) with libmpv.

## Next

1. **Core library.** A Go package with a small interface: open a
   repository, sync the index and packs, get a stream for a track, and on
   desktop prime and ingest. Built into the xcframework and a Mac test
   binary. Drive access uses MediaLayer's own Google client ID.
2. **Library builder on macOS.** Scan a Google Drive folder and write the
   SQLite index, header packs and art packs into `.medialayer` on the
   Drive. Gate: the 407-file test library builds without reading the audio.
3. **macOS player, first version.** Artists, albums, tracks and artwork
   from the index; playback with Now Playing and media keys; playlists.
   Gate: browsable in seconds from a cold start, and a track starts within
   about 2 seconds.
4. **macOS desktop features.** Menu bar control, cache cap with offline
   albums, write-mode toggle, the optional Tauon mount, a notarized build
   with Sparkle updates.
5. **iOS from the same project.** The iOS player check (framework size, 30
   minutes of locked-screen playback, a TestFlight upload), then the
   iPhone and iPad app reading the library the Mac built.
