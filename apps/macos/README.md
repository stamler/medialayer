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

## Songs table

- To show or hide a column, right-click the table header or choose
  View > Columns. To move a column, drag its header. The app keeps your
  choice. Restore Default Columns in View > Columns resets it.
- Album Artist, Composer, Disc, Bit Rate, Size and Date Modified are
  hidden at first. Name always shows.
- The Art column loads each cover when its row comes into view, and keeps
  at most 32 MB of thumbnails in memory.
- The Clouds column shows an icon for each source of the track:
  - The primary always holds the track.
  - A secondary icon shows at full strength when the secondary's last
    sync in this session finished and copied the index version the app
    shows. That secondary holds every track, except paths that sync can't
    keep apart (see Known gaps in [docs/sync.md](../../docs/sync.md)).
  - A dimmed icon means the secondary may not hold the track yet: it is
    copying, paused, failed, has an older version, or has not synced since
    the app opened. Hover over the icon to see why.
  - The app works this out from the sources' state and the index version.
    It writes nothing per source into the index.

## Sources

Add Source (Library menu, or the + by the chips) adds a second copy of
the library: another Drive folder, a folder in another Google account, or
a local folder. Right-click a chip to review a paused sync, stop syncing
to it, or make it the primary. See [docs/sync.md](../../docs/sync.md).

## Not yet

Search, playlists, the sidebar, the system Now Playing controls
(Control Centre) and media keys, header packs, the change feed, Proton,
iCloud and SMB sources, the sync lease, following a primary moved on
another device, and testing with 10,000 tracks.
