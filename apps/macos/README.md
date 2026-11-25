# MediaLayer for Mac (proof of concept)

A SwiftUI player on the Go core in `internal/core`: sign in to Google
Drive, pick the music folder, and the library indexes itself (only file
headers are read) into SQLite, with the index, cover art and the primary
record kept in a `.medialayer` folder on Drive. Browse with the column
browser (Genres, Composers, Artists, Albums) above the Songs table, and
double-click to play. Tracks stream from Drive through a localhost
endpoint in the core, one ranged request per play or seek.

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

## Not yet

Cover images stored as separate files in album folders (only embedded
art is read), search, playlists, the sidebar, Now Playing and media
keys, header packs, the change feed, more than one cloud, and testing
with 10,000 tracks.
