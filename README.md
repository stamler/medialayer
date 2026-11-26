# MediaLayer

MediaLayer makes a music and video library stored on cloud storage (Google
Drive, Proton Drive, iCloud Drive, SMB shares) feel local: desktop apps see a
mounted drive whose tags and cover art load instantly, while audio and video
stream from the cloud on demand. There is no server: the app and one or more
clouds are all a user needs.

Status: proof of concept. The rclone spike decided that MediaLayer is built on
rclone in Go (see [docs/spike.md](docs/spike.md)), and a macOS app on that core
now plays a Google Drive library. The plan is in
[docs/milestones.md](docs/milestones.md).

## What works now

The macOS app ([apps/macos](apps/macos/README.md)):

- **Google sign-in** with MediaLayer's own OAuth client, and more than one
  Google account.
- **Indexing.** You pick a music folder on Drive. The core reads only the
  file headers into a SQLite index. It resumes if interrupted, and later
  syncs index only new and changed files.
- **The `.medialayer` folder.** The core saves the index (as numbered
  versions), the cover art packs and the library record (`repo.json`) in
  the music folder. A new device restores the library from it in seconds.
- **Cover art.** The app uses embedded art first, then a cover image in the
  album folder (for example `cover.jpg` or `folder.jpg`).
- **Browsing and playback.** A column browser (Genres, Composers, Artists,
  Albums) sits above the Songs table. Tracks stream from Drive through a
  localhost endpoint, one ranged request per play or seek.
- **Search** from the toolbar (⌘F), across titles, artists, albums,
  composers and genres, ignoring case and accents.
- **Sidebar and playlists.** A collapsible sidebar (⌃⌘S) with playlists:
  create (⌘N), rename, delete, add songs by menu or drag, reorder. They are
  stored in the index, so they reach every source with the next sync.
- **Columns.** Album art and Clouds (which sources hold the track)
  columns, and a chooser for which columns show (right-click the header,
  or View > Columns).
- **Now Playing and media keys.** The track shows in Control Centre, and
  the keyboard's media keys and headphone buttons control playback.
- **Shortcuts.** Space plays and pauses, ⌘→ and ⌘← go to the next and
  previous track, and ⌘R updates the library.
- **Second sources.** Add Source copies the whole library to another Drive
  folder, in the same or another Google account, or to a local folder. Sync
  is one way, primary to secondaries. Removed and replaced files go to the
  secondary's trash for 30 days. A sync pauses for review when a
  secondary's removals and replacements over the last 24 hours would pass
  5% of the library's files or 30 files, whichever is smaller. Tag edits,
  where the audio is unchanged, don't count.
  Every secondary also gets the current index, so each one is a complete
  library. Only one device syncs a library at a time; another device
  shows who is syncing and syncs after it (see
  [docs/sync.md](docs/sync.md#the-sync-lock)).
- **Status bar chips.** One chip per source, with an icon for its type. The
  primary is highlighted, and each secondary shows its sync state.
- **Make Primary.** A synced secondary can become the primary, and you can
  switch back the same way.

How sync and promotion work, step by step: [docs/sync.md](docs/sync.md).

## Layout

| Path | What it is |
| --- | --- |
| `internal/core` | The Go core: connecting, indexing, publishing, sync to secondaries, promotion, streaming |
| `internal/index` | The SQLite index |
| `internal/media` | Tag and cover art reading |
| `cmd/mlcore` | The core built as a C library for the Swift app |
| `apps/macos` | The SwiftUI app |
| `backend/medialayer`, `cmd/mlspike` | The rclone overlay backend and spike binary |
| `scripts/build-mac-app.sh` | Builds the Mac app |

## Build and test

```sh
go test ./...
go vet ./...
scripts/build-mac-app.sh   # on a Mac
```
