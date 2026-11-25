# rclone spike

See `docs/milestones.md` for what comes next.

This spike decides whether MediaLayer is built on rclone in Go. It has four
checks, each with a pass mark. Checks 1, 2 and 3 need a Mac.

| Check | Pass mark | Status |
| --- | --- | --- |
| 1. Overlay plus mount with Tauon | Tauon's scan with stored headers is at least 10 times faster than plain `rclone nfsmount`, and playback starts normally | Passed: hours with plain `nfsmount`, about 3.5 minutes with the overlay storing each file's first MiB (397 FLAC plus 10 MP3 on Google Drive) |
| 2. Proton partial reads | Each small read fetches about one 4 MB block, not the whole file | Needs a Mac run |
| 3. Go on iOS | Replaced by an iOS player check: gomobile xcframework size, browsing the SQLite index, 30 minutes of locked-screen playback from Drive, a TestFlight upload | Not started |
| 4. rclone API churn | Building against nearby rclone releases needs only small changes | Passed: builds and tests pass unchanged on v1.72.0, v1.74.4, v1.75.2 and v1.76.0 |

## Decision

The built-in player comes first. The core uses rclone's backends as a Go
library, with a SQLite index for browsing and one ranged request per track
for playback. The mount is an optional compatibility layer for apps like
Tauon. Go stays the core language on every platform, iOS included, with a
small interface to Swift. The reasons, the full results and the iOS
assessment are in the architecture proposal:
https://claude.ai/artifact/4F35w22KCPz88bmpWkrhjK

What the Tauon run found:

- Plain `rclone nfsmount` makes one Drive request for every 32 KB block, so
  an import takes hours.
- With stored headers the cloud is not read during a scan. The floor is
  rclone's NFS server reopening the file for every 32 KB block, about 0.3
  seconds per file even on a local folder.
- Tauon reads about 1 MiB per file, because of Python's buffering and the
  1 MiB NFS block size macOS reports, so `prime` stores the first MiB.
- rclone's shared Google client ID is heavily throttled. Use your own.

## What is here

- `backend/medialayer`: an rclone overlay backend. It wraps any remote,
  stores the bytes before the audio of every FLAC file (tags, pictures,
  seek table), and answers reads that start inside those bytes locally. The
  cloud is contacted only if a reader goes past them.
- `cmd/mlspike`: the rclone binary with that backend built in. Every rclone
  command works.
- `scripts/churn.sh`: builds and tests the overlay against another rclone
  release.

The header store is a local directory standing in for the header packs that
will live in the cloud repository.

A stored header is used only while it still describes the file. The size
must match, the modification time must match exactly, and where the remote
has a content hash without reading the file (Google Drive, S3 and most
clouds), that must match too. The hash catches a retag that keeps the size
and the time, which FLAC padding and "preserve timestamps" options make
common. `prime` refuses a remote with neither times nor a quick hash.

One gap is known. If a file is replaced while a mount still holds its old
listing, a read can get the old header followed by the new file's audio.
The repository index will close this by pinning a file version (revision or
etag) where the remote has one.

## What the tests already show

Reads through rclone's VFS (what `nfsmount` and `mount` serve) were tested
with a local folder standing in for the cloud:

- With `--vfs-cache-mode off --buffer-size 0`, reading a file's tags never
  touches the cloud. This is the read-only configuration for Tauon.
- With the default `--buffer-size 16M`, or with `--vfs-cache-mode full`, the
  app still gets its answer from the stored header, but rclone reads ahead
  past it in the background: one cloud read per file scanned. `full` mode
  is needed for writes (tag editing through the mount), so write mode
  currently costs that read on a first scan. Removing it needs a small
  upstream rclone change (the full-mode downloader always reads at least
  1 MiB ahead).

## Running check 1 on a Mac

Build (Go downloads the toolchain it needs):

```sh
git clone https://github.com/stamler/medialayer && cd medialayer
go build -o mlspike ./cmd/mlspike
```

Set up remotes. The Google Drive step opens a browser to sign in:

```sh
./mlspike config create gdrive drive scope=drive.readonly
./mlspike config create ml medialayer remote=gdrive:Music/SpikeLibrary
```

`gdrive:Music/SpikeLibrary` is the test folder: 397 FLAC files plus 10 MP3s. Only
FLAC is primed, so Tauon reads the MP3s from the cloud on every import.

Baseline, plain rclone (unmount with `umount ~/mnt/plain` afterwards):

```sh
mkdir -p ~/mnt/plain
./mlspike nfsmount gdrive:Music/SpikeLibrary ~/mnt/plain \
  --vfs-cache-mode off --buffer-size 0 --read-only --dir-cache-time 1h
```

In Tauon, add `~/mnt/plain` as a music folder, time the import, and note
whether playback starts normally.

Prime the headers, then mount the overlay:

```sh
./mlspike backend prime ml: -v
mkdir -p ~/mnt/ml
./mlspike nfsmount ml: ~/mnt/ml \
  --vfs-cache-mode off --buffer-size 0 --read-only --dir-cache-time 1h \
  --rc --rc-no-auth -v -o readahead=0
```

`prime` keeps each file's metadata rounded up to the next whole MiB
(`store_to_block`, default 1 MiB). macOS reports a 1 MiB block size for NFS
mounts, and Tauon reads FLAC tags through a Python buffered file, so its
first read of every file pulls in the whole first MiB even though the tags
end earlier. Storing that MiB keeps the scan off the cloud, at about 1 MiB
per file (the metadata alone averaged about 180 KB in the test library).
Running `prime` again tops up records stored with less.

`prime` prints how many files it stored, how many range requests it made
and how long it took. In Tauon, remove the first folder, add `~/mnt/ml`,
and time the import again. Then, while the mount is running:

```sh
./mlspike rc backend/command command=stats fs=ml:
```

The counters are per open, not per file. Through `nfsmount` every NFS
READ (32 KiB on macOS) opens the file again, so one file read in 40 blocks
counts 40 opens. `served_locally` counts opens answered from the stored
header, `passed_through` opens that started past it, and
`streams_continued` how many cloud reads (of any file, primed or not)
continued a stream left open by the previous block instead of making a new
request. Tauon reads about 1 MiB of audio past the tags of each file, so
`passed_through` is expected to be high; most of those reads should show
up in `streams_continued`. The mount also logs
these numbers every 30 seconds with `-v`.

Mount with `-o readahead=0` (passed to `mount_nfs`, see `man mount_nfs`)
so the macOS NFS client does not read further ahead on its own.

Directory listings still come from Google Drive in this spike; the
repository index will answer them later. `--dir-cache-time 1h` keeps a
second listing local.

## Running check 2

```sh
./mlspike config create proton protondrive
./mlspike cat proton:Music/test.flac --count 65536 -vv --dump headers 2>&1 \
  | grep -E "^(GET|HTTP/|Content-Length)"
./mlspike cat proton:Music/test.flac --offset 50000000 --count 65536 -vv \
  --dump headers 2>&1 | grep -E "^(GET|HTTP/|Content-Length)"
```

Each read should show one block download of about 4 MB, not the whole file.
