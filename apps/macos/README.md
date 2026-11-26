# MediaLayer for Mac (proof of concept)

A SwiftUI player on the Go core in `internal/core`: sign in to Google
Drive, pick the music folder, and the library indexes itself (only file
headers are read) into SQLite, with the index, cover art and the primary
record kept in a `.medialayer` folder on Drive. Browse with the column
browser (Genres, Composers, Artists, Albums) above the Songs table, and
double-click to play. Tracks stream from Drive through a localhost
endpoint in the core, one ranged request per play or seek.

Search from the field in the toolbar (⌘F to focus it, Escape to clear
it). Each word you type must match the name, artist, album artist,
album, composer or genre, ignoring case and accents. The column browser
and the Songs table show only the tracks that match.

## Build

Needs Go and the Xcode Command Line Tools (full Xcode is not needed):

```sh
scripts/build-mac-app.sh
open apps/macos/MediaLayer.app
```

The Go core is linked as a C library (`cmd/mlcore`, built with
`-buildmode=c-archive`); gomobile and an xcframework replace this once
the iOS app needs Xcode anyway.

## Google sign-in

The app uses MediaLayer's own Google OAuth client, read for now from
`~/Library/Application Support/MediaLayer/google-oauth.json`:

```json
{"client_id": "....apps.googleusercontent.com", "client_secret": "..."}
```

It asks for full Drive access, because it writes `.medialayer` into the
music folder.

## Files

Everything local lives in `~/Library/Application Support/MediaLayer` and
is a cache: delete it and the app restores the library from `.medialayer`
on Drive.

## First results (SpikeLibrary, 397 FLAC + 10 MP3 on Google Drive)

- Indexing from scratch: about 2 minutes, no audio read.
- A restart opens the library from the local index at once.
- Playback starts in 1.0 to 2.1 s (status bar, "started in").

## Sources

Add Source (Library menu, or the + by the chips) adds a second copy of
the library: another Drive folder, a folder in another Google account, or
a local folder. Right-click a chip to review a paused sync, stop syncing
to it, or make it the primary. See [docs/sync.md](../../docs/sync.md).

## Now Playing and media keys

The app shows the playing track in Control Centre's Now Playing, and the
keyboard's media keys (F7 to F9), headphone buttons and Control Centre
control it. `NowPlaying.swift` does this through the system's
MediaPlayer framework:

1. When a track starts, the app publishes its title, artist, album,
   duration and cover, and turns on play, pause, next, previous and the
   scrubber.
2. When playback pauses or resumes, the app publishes the elapsed time
   and the rate. The system counts the time on from there.
3. When the position moves more than a second from that count, after a
   seek or a stall, the app publishes the elapsed time again.
4. When playback stops or the app quits, the app clears Now Playing and
   turns the commands off.

Media keys go to the app that played audio last, so press play in the
app once before you use them.

## Not yet

Playlists, the sidebar, header packs, the change feed, Proton,
iCloud and SMB sources, the sync lease, following a primary moved on
another device, and testing with 10,000 tracks.
