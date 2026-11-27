# SMB sources, Tailscale and the host helper

A design for the rest of milestone 4: an SMB share as a source, reached
over Tailscale or WireGuard if need be, with its password in the
keychain, and an optional helper on the machine that serves the share so
the slow parts of SMB are done at disk speed. Nothing here is built yet.
The rules it must keep are in [sync.md](sync.md).

The setup this is designed around: a Mac (or NAS) at home shares a music
folder over SMB, and other devices reach it over Tailscale, often from
outside the house, at 20 to 100 ms a round trip.

## Summary

1. **SMB source.** rclone's `smb` backend, which is pure Go (no mount, no
   Finder), so it works the same on macOS and on iOS. Add Source asks for
   the server, user name and password, then browses shares and folders.
   It can be a secondary, and, unlike Proton today, picked as the primary
   from the start, since a music collection usually already lives on the
   share.
2. **Password in the keychain.** Each SMB login is its own internet
   password item in the keychain. It is never written to the rclone
   config; the app hands it to the core in memory. Finder's saved
   password for the server can be imported.
3. **Networking.** First, the system's network, which includes the
   Tailscale app when it is running: nothing to build beyond clear errors.
   Then, as an option, MediaLayer's own Tailscale node, built in with
   `tsnet` (userspace WireGuard inside the core, no VPN, no system
   extension), and later a plain WireGuard config for people without
   Tailscale.
4. **Host helper.** A small program (`mlhost`) on the machine that serves
   the share. It watches the folder and keeps an inventory in the share's
   `.medialayer/host/`: every file's size and time, the indexer's parsed
   tags and art, and its audio hash. A device indexing the share takes
   tags and art from it instead of reading every file's header over the
   network, and the hash pass needn't download the library. The helper
   never writes the index, never takes the sync lock and never changes a
   music file, and nothing it says decides a removal, a replacement, the
   daily limit or whether a secondary is whole: those still come from a
   real SMB listing. Using its inventory in place of that listing is a
   second level, worth building only if measuring shows listing is the
   slow part, and it needs stricter rules (below).

The order I'd build it in: SMB with the keychain over the system network,
then a measurement of Dean's library over Tailscale, then the helper,
then built-in Tailscale.

## 1. The SMB source

### What rclone gives us

rclone 1.76's `smb` backend (`backend/smb`, on `github.com/cloudsoda/go-smb2`)
speaks SMB 2 and 3 itself. Options that matter: `host`, `port`, `user`,
`pass`, `domain`, `spn`, `use_kerberos`, `case_insensitive` (default
true), `idle_timeout`. Paths are `share/folder/...`. Its behaviour:

- **No hashes.** `Hashes()` is empty, so sync compares by size and
  modification time only, and a file whose time alone differs can't be
  checked without reading it (on Drive the listing's MD5 settles it).
  The helper's hashes fill this gap (below).
- **Times are reported to the millisecond whatever the server's disk
  keeps.** `Precision()` is always 1 ms. A share on HFS+ (1 second) or
  exFAT or FAT (down to 2 seconds) rounds every time a copy sets, so every
  copied file would differ on the next sync, with no hash to settle it,
  and each would become a replacement that trips the daily limit. See
  "What the core needs for SMB".
- **Partial uploads, for copies only.** The backend says
  `PartialUploads`, so `operations.Copy` (music files) writes to
  `<name>.<8 hex>.partial` and renames it when done
  (`fs/operations/copy.go`). But the library's own files go through
  `operations.RcatSize` (`putBytes` in mirror.go, `upload` in repo.go),
  which calls `Put` directly, and the backend's `Update` opens with
  `O_TRUNC`, writes in place and removes the file on failure. So a cut
  connection can leave `repo.json` or `member.json` truncated or gone.
- **Listing is one folder at a time** (no `ListR`). rclone walks folders
  with several checkers at once, but each folder is at least one round
  trip, and a library is often a folder per album. Over Tailscale from
  outside the house that is the slow part of every sync, before a single
  header is read.
- **Range reads work** (`Open` with a `RangeOption`), so indexing and
  streaming work as on Drive, each read being an open, a read and a close.

### Source and config

A source is `{ID, Remote, Folder}` (`mirror.go`). An SMB source gets an
rclone remote of type `smb` named `smb`, `smb2`, ... holding the host,
port, user and domain, and `Folder` is `share/path`. What is new:

- `sourceType()` already returns `smb` from the config; `caseBlind()`
  becomes true when the remote's
  `case_insensitive` is (the default, and right for macOS and Windows
  servers; a Samba server set case-sensitive says false).
- `openSource` (and `ListFolders`, which opens remotes itself) builds the
  remote with the password added in memory, as an rclone connection
  string override (`smb2,pass=<obscured>:share/Music`), so the password
  is never saved in `rclone.conf`. The password comes from the app (see
  section 2). If the core has none for that remote, the open fails with
  "MediaLayer needs the password for <user> on <host>", which the app
  turns into a prompt. Two side effects to handle:
  - rclone names an Fs opened with overrides `smb2{hash}`, which changes
    `fs.ConfigString`. Audio hash keys (`audioKey`) and the stale-lock
    watch (lease.go) are keyed by it, so a new password would reset
    both. They should key SMB sources by remote name and folder instead.
  - rclone logs the remote string at debug level when it creates a
    backend, and the core's debug logging is on by default, so the
    obscured (and reversible) password would reach the log. The log
    must redact `pass=`, or the password goes in another way, such as
    a config storage that layers an in-memory value over the file.
- `remoteConnected` is true for `smb` when the app has given a password.
- Add Source: SMB asks for the server (a name, `host.local`, a tailnet
  name such as `studio.tailnet-name.ts.net`, or an address), user name
  and password, tries them, then lists shares (rclone lists shares at the
  top of an smb remote) and folders, as the Google and Proton pickers do.
- Choose Music Folder gains SMB, so an SMB folder can be the primary from
  setup. Today it browses only the first Google account. The indexer and
  sync already work with any `fs.Fs`; what is needed is the picker and
  `PrimaryRemote` naming an smb remote, as `localPrimary` does for a
  local folder today.

### What the core needs for SMB

Found by checking the core against the backend; each is small, but sync
isn't safe on SMB without them:

1. **Library files written whole.** `putBytes` and `upload` write to a
   temporary name beside the file (`name.<8 hex>.partial`) and then
   move it over the real name, on backends that say `PartialUploads`
   (SMB, local disks). A cut write then leaves the old `repo.json` or
   `member.json` in place. A server that refuses the rename because
   another device has the file open (Windows share modes) gets the file
   written in place, as before.
2. **Time precision measured.** Each sync of an SMB secondary writes its
   probe with a fixed time (an odd second with a fraction, in 2020),
   reads it back, and takes the precision it sees as the modify window
   for comparing that folder's files, a step loose where the disk rounds
   up. A server that doesn't keep the time it is given is refused as a
   secondary: with no hashes either, a retag that kept the size would go
   unseen.
3. **Leftover partial files ignored.** A file named like rclone's
   partial uploads (`*.<8 hex>.partial`) is neither copied, nor trashed,
   nor counted toward the daily limit. A secondary's sync removes those
   left in it, since only MediaLayer writes there; a primary's are left
   alone. Restore skips them among the art packs.
4. **The snapshot without hashes.** On a cloud with neither MD5 nor
   SHA-1, a snapshot is this device's when its size matches and its
   modification time is within 2 seconds of the local file's. An upload
   gives the remote file the local time and a restore gives the local
   file the remote time, so another device's index of the same number
   and size is told apart unless both were written within 2 seconds,
   which the sync lock makes unlikely. Without this, every sync would
   upload the snapshot again, and an SMB primary would make a new index
   version on every sync.

### The sync rules on SMB

The rules of [sync.md](sync.md) apply unchanged once the core changes
below are made. Things to note:

- **The sync lock** is files in `.medialayer`, which SMB handles well
  (it can even create a file only if absent, though the lock doesn't need
  that).
- **The overlap check** (a probe file in the secondary must not appear in
  the primary) also catches a share reached two ways, such as by its
  `.local` name and its tailnet name, or an SMB folder that is also a
  local folder on this Mac.
- **Unicode.** A Mac server keeps names as they were written; paths are
  already matched in NFC.
- **A share mounted in Finder** (`/Volumes/Music/...`) already works as a
  local folder today, on the Mac only. The SMB source replaces that: it
  needs no mount, works on iOS, and doesn't break when the Mac sleeps and
  the mount goes stale.

## 2. The password in the keychain

Today the rclone config holds every sign-in and is encrypted under a key
kept in the login keychain (`configkey.go`, `Keychain.swift`). An SMB
password could go into that file too and be just as safe, but it would
be invisible to the user and tied to one install. A keychain item of its
own is better for SMB:

- **The item.** `kSecClassInternetPassword` with `kSecAttrServer` (the
  host as typed), `kSecAttrAccount` (the user), `kSecAttrProtocol`
  `kSecAttrProtocolSMB`, `kSecAttrPort` when it isn't 445,
  `kSecAttrSecurityDomain` for a Windows domain, and a label such as
  "MediaLayer: dean on studio". The user can see, change or delete it.
- **Which keychain: the login keychain.** macOS has two. The config key
  (`Keychain.swift`) and Finder's saved SMB passwords are in the older
  file-based login keychain, shown in Keychain Access, and that is where
  the SMB items go. iCloud Keychain sync (`kSecAttrSynchronizable`)
  works only in the data-protection keychain
  (`kSecUseDataProtectionKeychain`), which needs a
  `keychain-access-groups` entitlement and so a provisioning profile from
  the paid Apple Developer Program, which MediaLayer doesn't use (see
  [milestones.md](milestones.md)). So the password doesn't follow the
  user to the iPhone: the iOS app asks for it once and keeps it in its
  own keychain. The keychain code stays in one small Swift type, so
  moving to the data-protection keychain later, if that decision
  changes, means moving the items in one place.
- **Signing.** The login keychain trusts an item's app by its code
  signature. An ad hoc signature changes with every build, so macOS
  would ask "MediaLayer wants to use your confidential information"
  again after each update. The app is signed with one stable
  self-signed identity, so it asks once.
- **Finder's saved password.** Add Source can offer "Use the password
  saved for this server", reading Finder's item in the login keychain
  and saving a copy as MediaLayer's own item; macOS asks the user
  whether MediaLayer may read it. To test: whether that prompt appears
  for an app outside the Mac App Store. If not, the user types it.
- **How the core gets it.** The core never reads the keychain (it is Go,
  and gomobile on iOS can't use the Security framework from Go easily).
  At launch, and when a source is added, the app reads the item for each
  smb remote and calls a new `Core.SetSMBPassword(remote, password)`;
  the core keeps it in memory only. A remote whose item is gone is shown
  as needing its password, like a Proton session that ended.
- **Kerberos** (`use_kerberos`) needs no password and could come later
  for enterprise shares. Not planned.

## 3. Tailscale and WireGuard

### The system network first

When the Tailscale app runs on the device, a tailnet name or a
`100.x.y.z` address resolves and connects like any other; rclone's
dialer (`fshttp.NewDialer`) needs nothing. So the first version builds
nothing for Tailscale except errors that say what is wrong: when the
server is a `*.ts.net` name or in `100.64.0.0/10` and can't be reached,
the message says "Can't reach studio over Tailscale. Is Tailscale
connected on this Mac?" rather than a timeout.

### MediaLayer's own Tailscale node

`tailscale.com/tsnet` runs a whole Tailscale node inside a Go program:
WireGuard and a TCP/IP stack in user space. The program dials tailnet
addresses through it, and nothing else on the device uses it. This is
what makes "Tailscale built in" possible:

- **No Tailscale app needed.** On an iPhone this also means MediaLayer
  works while another VPN is on (iOS allows one VPN at a time, and the
  Tailscale app is a VPN). On a child's device (milestone 6) the parent
  needn't install Tailscale at all, only share the one server.
- **Sign-in.** The node starts, Tailscale returns a login URL, the app
  opens it, the user approves the device in their tailnet, and the node
  appears in the admin console as `medialayer-<device name>`. An auth key
  pasted in instead also works. A custom control server (Headscale) is
  the `ControlURL` setting.
- **Reaching the server.** rclone's smb backend has no way to take a
  custom dialer, so the core runs a small forwarder: it listens on
  `127.0.0.1:<port>` and passes each connection to
  `tsnet.Server.Dial(ctx, "tcp", "studio:445")`. The opened remote's
  `host` is `127.0.0.1` and `port` the forwarder's, given as overrides
  at open time, while the saved remote keeps the real host for display,
  the keychain and cache keys. The port is kept per source so it is the
  same on every launch. Any process on the device can connect to a
  loopback port, so other apps (or other users of the Mac) could reach
  the server's port 445 through MediaLayer's node while it runs; they
  still need the SMB password, but it bypasses the tailnet's intent that
  only MediaLayer reaches it. A Unix socket would limit that, if go-smb2
  can be given a connection directly, which needs a dialer hook in
  rclone's backend: worth proposing upstream, as it removes the
  forwarder entirely.
- **Its state.** The node's private key and login are tsnet's state.
  They go through a custom `ipn.StateStore` into the core's encrypted
  config (or a file encrypted with the same keychain key), never a plain
  file in Application Support. tsnet still writes logs under its `Dir`,
  and by default sends them to Tailscale's log service; MediaLayer turns
  that off (`TS_NO_LOGS_NO_SUPPORT`, or tsnet's `Logf` and log upload
  settings) unless the user asks for support logging.
- **The tailnet's rules.** The node is a device of its own, so the
  tailnet's ACLs must let it reach the server on port 445, device
  approval may need the admin, and its key expires (180 days by default)
  unless expiry is turned off for it. The app shows "Tailscale sign-in
  expired" with a button to sign in again.
- **Costs.** `tailscale.com` is a large module: expect the core to grow
  by roughly 10 to 20 MB per platform slice, which matters for the iOS
  framework size check in milestone 7, and Tailscale tends to need the
  newest Go release. A tsnet node uses a few tens of MB of memory. To
  measure before deciding: binary size, memory on an iPhone, and stream
  start time through the node versus the Tailscale app.
- **Which path.** Per SMB source: "Network: this device's (including the
  Tailscale app)" or "MediaLayer's Tailscale". Default to the device's;
  offer MediaLayer's when the server is a tailnet name that the device's
  network can't reach.

### Plain WireGuard

The same forwarder works with `golang.zx2c4.com/wireguard` and its
`tun/netstack` package, from a `wg-quick` config the user imports (a
file, or a QR code on iOS): for people whose router runs WireGuard. It is
much smaller than tsnet, but there is no sign-in, no name lookup beyond
the config's DNS, and the config's private key needs the same keychain
protection. Later, if asked for.

## 4. The host helper

### What is slow, and what the helper can do about it

Over SMB, from outside the house:

| Work | Over SMB | On the host | Helper level |
|---|---|---|---|
| Indexing new files (primary) | several round trips per file, plus tags and art | disk speed | 1 |
| The audio hash pass (primary) | downloads the whole library | disk speed | 1 |
| Listing the library (every sync, primary or secondary) | a round trip per folder | under a second for 20,000 files | 2 |
| Copying files to and from it | network speed | no help: the bytes must move | none |

So level 1 makes the first index of a large share and the hash pass
nearly free, without touching how sync decides anything. Level 2 would
also make each sync's listing nearly free, at a real cost in safety
reasoning. Neither makes copies faster.

Step 0 is to measure this on Dean's library with `mlspike` over
Tailscale: time to list, time to index from scratch, and the round trip
time. If listing a few thousand folders takes seconds, not minutes,
level 2 isn't needed.

**Measured** (27 November 2026, `cmd/smbtime`, read-only): Dean's music
folder on OpenMediaVault 7 (Debian 12, Samba), from a MacBook over
Tailscale, as a read-only SMB user.

| | Result |
|---|---|
| Library | 2,058 folders, 22,808 files, 16,821 tracks, 483 GB |
| Round trip | about 33 ms (a lookup, open and close, took 66 ms) |
| Listing everything, 8 folders at once | 45 s |
| Track headers, 8 at once | 17.6 a second, 383 ms each, 278 KB a track (embedded art) |
| So a first index | about 16 minutes |
| Starting a track (first 256 KB) | 337 ms |
| Reading a track in full | 1.5 MB/s |
| So the audio hash pass | about 90 hours |

What follows from it:
- Level 1 is worth building: it takes the hash pass from days to the
  disk's speed (an hour or two for 478 GB) and the first index from 16
  minutes to about one.
- Level 2 isn't needed yet: 45 s of listing a sync is tolerable.
- The server is about 2,000 km from the Mac, so the 33 ms is distance,
  and 1.5 MB/s (about 12 Mbit/s) is most likely the upload speed at the
  server's site; a direct Tailscale path wouldn't change either. So for
  this library, anything that reads every file must run on the server:
  the hash pass (helper level 1), and copies to a cloud secondary, which
  through the Mac would take as long as the hash pass and which the
  helper can't help. The server uploading itself (the headless device
  under "What it could do later") moves up from an idea to the way to
  give this library a Drive or Proton secondary. High-resolution files
  (24-bit/192 kHz, about 1.2 MB/s) would be at the edge of playing
  smoothly from this server.
- The indexer read headers with `io.ReadAll`, whose growing buffer makes
  a dozen or more reads for one range, each a round trip on SMB; it now
  reads a range in one buffer, as `smbtime` does. Playback still copies
  in 32 KB reads, which on SMB is a round trip each (about 1 MB/s at
  33 ms); an SMB source needs read-ahead there.
- A read-only user only sees files whose permissions let others read
  them; two folders and some files were left out.

### What it is

`mlhost`, a single Go binary from this repo (`cmd/mlhost`), using the
core's own `internal/media` parsers and art pack code, so its rows are
what a device's indexer with the same parser version would make. It
runs as a launchd agent on a Mac (`mlhost install --folder ~/Music/Library`), a systemd service
on Linux, or a Docker container on a NAS. It reads the library folder
from its own disk and writes only inside `<folder>/.medialayer/host/`.
It needs no network port, no pairing and no password: the share's own
access control covers what it writes, and anyone who can write there can
already change the music.

### What it writes

In `.medialayer/host/`:

- `inventory.sqlite`: one row per file in the folder (including nested
  `.medialayer` folders, which the overlap check looks for), with its
  path, size, modification time in milliseconds as SMB will report it,
  file id (inode), SHA-256 of the content, `audio_sha256` with its hash
  version, and for media files the parsed `media.Info` and the SHA-256 of
  each picture, as the indexer stores them. The file also holds its own
  sequence number and the helper's versions, so one read gives a
  consistent whole. It is written as a new file and renamed into place,
  and the device reads it in one open, so it gets the old or the new
  one, never half.
- `art/`: art packs in the core's own format, content-addressed by
  SHA-256, so a device copies the images it lacks straight into its own
  packs.
- `status.json`: the helper's id, version, format, parser and hash
  versions, the inventory's sequence and time, the probes it has seen,
  and the files it couldn't read.

It keeps these current with FSEvents (macOS) or inotify (Linux), plus a
full rescan at start and once a day. A watcher that reports it lost
events (FSEvents' "must scan subdirectories", inotify's queue overflow)
stops the helper vouching for anything until a rescan ends. A changed
file's row loses its hashes and tags at once, before it is read again.
Hashing runs in the background at low priority; a row without hashes is
unknown, not wrong.

**Paths.** The helper sees names as they are on its disk; a device sees
them after the SMB server's mapping (macOS and Samba's `fruit`/`catia`
turn `:` and similar into private-use characters) and rclone's smb
encoding. The helper applies the same mappings and records each path as
the device will list it. A row whose path doesn't match is just not
used, so a mistake here costs speed, not safety, at level 1.

**Versions.** "The same rows the device would make" holds only for the
same parser. The parser version is part of the index's rows' meaning: a
device takes tags and art only from a helper whose parser version
matches its own, and audio hashes only with a matching hash version.

### Level 1: tags, art and audio hashes

This is the level I'd build. The device lists the folder over SMB as it
does today, so every sync decision (what to copy, replace or remove, the
overlap and empty-or-copy checks, `member.json`'s `synced`) rests on a
real listing, and the helper changes none of sync.md's guarantees.

- **Indexing.** For a new or changed file whose path, size and time match
  an inventory row with parsed info, the indexer takes the row's tags and
  art instead of reading the file's header. The device still writes the
  index's rows itself, under the lock, in its own versions; the helper
  never makes an index version or touches playlists. A row that doesn't
  match is read over SMB as today. The worst a wrong row does is a wrong
  tag shown, fixed when the file changes or the helper is turned off.
- **Audio hashes** go into the index as `audio_sha256`, so the hash pass
  needn't download the library. They are not put in the device's
  `audio-hashes.json` and are not used to decide that a replacement is
  only a tag edit: that decision exempts changes from the daily limit,
  and sync.md already refuses to trust `audio_sha256` there, since a row
  keyed by path, size and time can't tell a file rewritten with its size
  and time kept. So at a pause decision the device still hashes the
  files itself, as today.
- **Content hashes** are not used for sync's equality check. rclone's
  `operations.Equal`, finding equal hashes, sets the secondary's time to
  match, so a wrong hash would hide a real difference for good. The
  SHA-256 is used only by `matchMoves`, to keep a moved file's id, where
  a wrong one at worst gives a track the wrong id.
- **Freshness** matters little at this level, since every row is matched
  by path, size and time against the real listing. The device fetches
  the inventory only when it has files to index or hash.

The gain: the first index of a big share, and the hash pass, at disk
speed. Every later sync still lists over SMB.

### Level 2: the inventory as the listing

Only if step 0 shows that listing over SMB is too slow. Here the
inventory stands in for the real listing, which is what sync's safety
rests on, so it needs proof that the inventory is of this folder and
current, and limits on what it may decide.

**The check.** The device writes a probe to `.medialayer/probe/` with
one name per device (as mirror.go reuses one probe name, rather than a
new file each sync) and new contents each time. The helper records the
probe's path relative to its own folder and its contents in
`status.json`, after handling every change queued before it, with the
inventory sequence that includes them; the device waits up to about 10
seconds, then reads an inventory of that sequence or later. This shows
the helper's folder is this folder (by the path relative to its root),
that it was alive a moment ago, and that it had seen every change that
reached its disk before the probe. It doesn't show that its disk is the
truth: a copy kept in step by a sync tool (Syncthing, say) passes while
it lags. And it writes to the folder, which sync.md avoids for checks
on a primary, so a full primary fails it and falls back to listing.

**What it may not decide.** A real listing over SMB is still made:

- for the overlap and empty-or-copy checks, made after the secondaries'
  probes are written (as now, the primary is listed again when a probe
  is new);
- for any sync of a secondary that will write `member.json` as `synced`
  after removing or replacing files, and for the syncs Make Primary runs,
  so a secondary marked whole, or promoted, was checked file by file;
- every tenth sync of a folder, and the first after the helper restarts,
  compared with the inventory; any difference beyond changes made between
  the two looks turns level 2 off for that folder, with a notice.

Before any removal, the file's absence is confirmed on the side that
decides it, matching paths as `mirror()` does (NFC, and ignoring case
where the folder is case-blind): on an SMB primary, the index's file is
looked up there before it is marked gone; for a secondary, the primary
is checked for the file the secondary would lose.

**What it can still get wrong.** A secondary's inventory that lists a
file it no longer has, at the right size and time, leaves the file
missing until the next real listing. That is why `synced` after changes,
and Make Primary, need a real listing: an incomplete secondary is the
one thing sync.md says must not be marked whole. Someone who can write
to the share could also fake the helper's answers by polling for the
probe, which makes level 2 a speed feature for a trusted host, not a
check of the host.

**Fetching it.** 20,000 rows with tags and picture hashes is perhaps 10
to 30 MB, read in full each sync. Over Tailscale that can take longer
than listing, so level 2 needs a journal: a base inventory rebuilt daily
and a small file of changes since, of which the device reads only the
part it lacks. Step 0's measurement should time this too.

### What it could do later

The helper sees changes as they happen, so it could tell the device to
sync (a "changed" flag in `status.json` a device polls cheaply) instead
of waiting for app start or Update Library. Further out, the always-on
machine could run the core itself as a headless device: hold the sync
lock and copy the library to its Drive and Proton secondaries at disk
speed, with no Mac awake. That is a bigger change (a device without a UI,
its own sign-ins) and isn't part of this design.

## Open choices

1. **Built-in Tailscale: when?** I'd build SMB over the system network
   first and add MediaLayer's own Tailscale node after, once the binary
   size and iPhone memory are measured. The other way round makes the
   first SMB version wait on a large dependency.
2. **SMB as the primary in milestone 4.** I'd allow it from the start,
   since that is where the helper pays off and where Dean's music already
   is. It means adding SMB to Choose Music Folder.
3. **Which keychain.** Decided: the login keychain, as the config key
   is today, since iCloud Keychain sync needs the paid Apple Developer
   Program.
4. **The helper's output: files in the share, or a service on the
   tailnet.** I'd write files in `.medialayer/host/`: no port, no
   pairing, no second set of credentials, and it works through any route
   to the share. A service could answer faster and push changes, but
   needs its own authentication.
5. **Level 2 of the helper (the inventory as the listing).** I'd wait
   for the measurement. It is where the safety argument gets thin, and
   level 1 already takes the first index and the hash pass off the
   network.
6. **Plain WireGuard.** Worth building only if someone needs it; Dean's
   setup is Tailscale.

## Steps

1. SMB remote, the core changes under "What the core needs for SMB",
   Add Source and Choose Music Folder, password in the keychain, over the
   system network. Tests against a Samba server in a container: a
   secondary built from scratch matches the primary, a cut upload of a
   track or of `member.json` leaves the old file whole, a share on a
   disk keeping 2-second times doesn't churn, the overlap check catches
   the same share by two names.
2. Measure Dean's library over Tailscale: listing, indexing, stream start.
3. `mlhost` level 1: tags, art and audio hashes; tests where the helper
   is stopped, behind, on another parser version, or wrong about a file,
   each ending with the same sync decisions as without it.
4. MediaLayer's own Tailscale node through the forwarder, its state in
   the encrypted config, sign-in from the app.
