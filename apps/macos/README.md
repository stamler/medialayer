# MediaLayer for Mac (proof of concept)

A SwiftUI and AppKit player on the Go core in `internal/core`: sign in to Google
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

## Songs table

- To show or hide a column, right-click the table header or choose
  View > Columns. To move a column, drag its header. To resize a column,
  drag the edge of its header. The app keeps your choices. Restore
  Default Columns resets the columns, their order and their widths.
- Album Artist, Composer, Disc, Bit Rate, Size and Date Modified are
  hidden at first. Name always shows.
- Click a column header to sort by it. Click it again to reverse the
  sort. The previous sort breaks ties.
- Double-click a song or press Return to play it. Type the first letters
  of the sorted column's text to jump to a song.
- Songs and each playlist keep their own scroll position.
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

## Sidebar and playlists

The sidebar (View > Show/Hide Sidebar, ⌃⌘S, or the toolbar button)
lists Songs and the playlists. To use playlists:

1. Choose File > New Playlist (⌘N) or click + under the sidebar. Type a
   name and press Return.
2. Add songs. Right-click rows in the table and choose Add to Playlist,
   or drag the rows onto a playlist in the sidebar. A song that is in the
   playlist already is not added again.
3. Select the playlist. The table shows its songs in playlist order.
   Drag rows to reorder them. Press Delete, or right-click and choose
   Remove from Playlist, to remove songs.
4. Double-click a song to play it. Playback continues through the
   playlist.
5. Right-click a playlist to rename or delete it. Deleting a playlist
   keeps its songs in the library.

Click a column header to sort a playlist; dragging to reorder then
stops until you choose View > Sort by Playlist Order. The search field
filters a playlist too: matching songs keep playlist order, and
dragging to reorder stops until you clear the search.

Playlists are stored in the library's index, and the Go core makes every
change to them:

1. A change marks the index as changed.
2. Five seconds after the last change, the app starts a sync. If a sync
   is running, the app waits for it to end first.
3. The sync publishes a new index version to the primary and every
   secondary.

A song whose file is moved or renamed stays in its playlists: the
indexer matches the file at its new path by size and SHA-256 (or, with
no hash, by size, modification time and name). A song whose file leaves
the library keeps its place but is hidden, and comes back if the file
returns. To drop hidden songs, right-click the playlist and choose
Remove Missing Songs.

Choose Music Folder refuses to switch while playlist changes are not on
the primary yet, and the message says what to do:

- Update the library first, or delete the playlists to discard the
  changes.
- If the primary's cloud is full, an update can't save them. Free up
  space there and update the library, or delete the playlists.

Known limits:

- A file moved onto a path that the index knows as missing (a file that
  was there before and was removed) takes that old file's id, not its
  own. The moved song leaves its playlists.
- Each burst of playlist edits starts a full sync, which lists the
  library and every source. A job that only publishes the index is
  needed.
- After a restore, a carried playlist that you edit before indexing
  ends keeps your version: songs the restored index didn't list yet
  aren't added back to it.

## Now Playing and media keys

The app shows the playing track in Control Centre's Now Playing, and the
keyboard's media keys (F7 to F9), headphone buttons and Control Centre
control it. `NowPlaying.swift` does this through the system's
MediaPlayer framework:

1. When a track starts, the app publishes its title, artist, album,
   duration and cover, and turns on play, pause, next, previous and the
   scrubber.
2. When playback pauses, resumes or finishes buffering, the app
   publishes the elapsed time and the rate. The rate is 1 only while
   audio plays, so it is 0 while the track buffers. The system counts the
   time on from there.
3. When the position moves more than a second from that count, after a
   seek or a stall, the app publishes the elapsed time again.
4. When playback stops or the app quits, the app clears Now Playing and
   turns the commands off.

Media keys go to the app that played audio last, so press play in the
app once before you use them.

## How the window stays fluid

The window is SwiftUI, except for the Songs table and the column
browser's four lists. Those are AppKit table views (`NSTableView`) with
plain, reused cells. SwiftUI's own table and lists made a view per cell
and rebuilt them on each change, which made resizing, searching and the
sidebar animation stutter.

The app keeps each change local:

1. The core's status, polled every half second, is in `StatusModel`.
   Only the status bar, the source chips, the + under the sidebar and the
   menus redraw when it changes.
2. The search text and the browser choices announce changes through
   `BrowseSignal`. Only the search field, the column browser and the
   status bar redraw on a keystroke.
3. The playback position is in `PlaybackClock`. Only the now-playing
   display and the "started in" text redraw as it ticks.
4. The Songs table gets its data from `AppModel.tableFeed`, not from
   SwiftUI. After any change, AppModel sends the feed one update at the
   end of the run loop turn, and only if the shown tracks, the sort, the
   playing track or the sources changed. The table then:
   - redraws only the rows whose track changed, if the rows are the same;
   - reloads, if the rows changed. A reload creates cells only for the
     rows on screen.
   - redraws the Art cells of the old and the new playing track;
   - redraws the Clouds cells when the sources change.
5. AppModel sorts the library once for each change of the tracks or the
   sort. The search and the browser only filter that sorted list. Each
   browser list is computed once for each change of the search or of a
   choice to its left.

## Testing with a large library

To try the window with 10,000 tracks without a large library, start the
app from Terminal with `MEDIALAYER_FAKE_TRACKS`:

```sh
MEDIALAYER_FAKE_TRACKS=10000 apps/macos/MediaLayer.app/Contents/MacOS/MediaLayer
```

1. The app reads the library from the index as usual.
2. It repeats the tracks until there are 10,000. Each copy gets a
   distinct id, and its artist, album artist and album get the copy's
   number, so the column browser grows too.
3. The copies exist only in the app's memory. The app writes nothing
   about them to the index or the core.
4. A copy plays its original's file. Adding a copy to a playlist adds its
   original.

Check that the table scrolls smoothly, that the window resizes smoothly,
that typing in the search field doesn't lag, and that the sidebar shows
and hides smoothly.

## Not yet

A publish-only job for playlist changes (see Known limits above),
merging playlist changes made on two devices, header packs, the change feed, Proton,
iCloud and SMB sources, the sync lease, following a primary moved on
another device, and testing with a real library of 10,000 tracks.
