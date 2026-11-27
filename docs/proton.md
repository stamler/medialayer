# Proton Drive: what was built, why, and how

This is the story of adding Proton Drive to MediaLayer, kept so the work
can be picked up later. The rules a sync follows are in
[sync.md](sync.md#proton-drive); this file covers the reasons behind them,
what went wrong on the way, and what is still open.

## What Proton can do in MediaLayer

- **As a secondary.** Add Source puts a copy of the library in a Proton
  Drive folder, with the same rules as a Google Drive secondary: one-way
  sync, a daily limit on removals and replacements, and a trash folder.
- **As the primary.** Once a Proton folder is synced, Make Primary can
  switch the library to it. Proton keeps files in encrypted 4 MB blocks,
  so reading tags or starting and seeking a track fetches and decrypts at
  least one block. Indexing and starting tracks are slower than on Drive.
  Choose Music Folder still browses only Google Drive, so the only way to
  get a Proton primary is Make Primary.
- **Limits:**
  - **One account.** rclone's Proton backend keeps the config that a
    renewed session is saved to in a package variable, so with two Proton
    accounts open at once, one account's session could be saved under the
    other's name. MediaLayer allows one Proton account at a time. Keeping
    the config per remote upstream would lift this.
  - **Case-insensitive paths.** Proton isn't known to keep names apart by
    case alone, so paths are matched ignoring case, as on local disks.
  - **Modification times to the second.** Proton keeps a file's time from
    its upload and can't change it later. A file whose time alone differs
    from the primary's, with the same size and SHA-1, counts as the same
    file. After Make Primary, files whose times differ by less than a
    second keep their ids, so nothing is read again.

## Credentials are encrypted with a key in the keychain

A Proton session includes the salted key password that unlocks the
account's encryption keys, so a stolen config file would open the
account's files. The Mac app therefore encrypts the whole rclone config,
Google's tokens included, with rclone's own config encryption, under a
random key kept in the login keychain. Proton's own Drive app for Mac
keeps its session in the keychain too. The file alone opens nothing. If
the key is lost, the core sets the unreadable file aside and the clouds
need connecting again. The password itself goes only to Proton and is
never kept.

## Our own sign-in, and why rclone's couldn't do security keys

Dean's Proton account uses FIDO2 security keys only (YubiKeys, and
passkeys in iCloud Keychain), with no authenticator app.

**Why rclone's sign-in failed.** rclone's Proton backend handles only a
6-digit code from an authenticator app for the second step. With a
security-key account it finishes the first (password) step and stops,
which leaves a session that isn't fully signed in. Proton then refuses the
first real request:

```
403 GET https://drive-api.proton.me/core/v4/users:
Access token does not have sufficient scope (Code=9100, Status=403)
```

**What the core does instead.** It signs in itself, with go-proton-api
(the library under rclone's backend), and hands the finished session to
rclone's backend. For a security key it does what Proton's Bridge does:

1. It reads the WebAuthn options Proton sends with the first step.
2. It builds the client data (`clientDataJSON`) the way Bridge does.
3. It talks to the key directly over USB HID (CTAP2), using
   `go-ctap/ctaphid` on top of `hidapi`, and has the key sign Proton's
   challenge.
4. It sends the signature back as the second step.

go-proton-api's `FIDO2Req.CredentialID` is a string, but Proton wants an
array of numbers. So a resty pre-request hook swaps in the right body for
that one call, and only while the key step runs.

**The flow the user sees.** Dean asked for the flow most people know from
browsers, with two touches around the PIN:

1. Username and password, then Sign In.
2. If Proton asks for the key, a screen with no text fields says "Touch
   your security key". This touch picks the key:
   - On CTAP 2.1 keys it uses `authenticatorSelection`.
   - Older keys (FIDO_2_0 / 2_1_PRE) get the dummy makeCredential that
     libfido2 uses, with an empty pinAuth. That waits for a touch, costs no
     PIN try and creates no credential.
3. A PIN step. The username is shown read-only, and the PIN has a field
   of its own. Only clicking Continue sends it; Return does nothing.
4. A second "Touch your security key to sign in" screen, again with no
   fields. This touch signs.

**Protection against typing the key's code.** A YubiKey touched while a
text field has focus types a 44-character one-time code and presses
Return. On the first try this landed in the PIN field and cost a PIN try.
Three things now prevent that:
- No text field has focus while the key waits.
- Return doesn't submit the PIN.
- A run of 32 or more letters from the YubiKey's modhex alphabet
  (`cbdefghijklnrtuv`, either case) is refused in both Swift and Go,
  before the key or Proton sees it.

**PIN retries.** A sign-in that stops to ask for more (a code, the mailbox
password, the PIN, or the PIN again after a wrong one) is kept at Proton
for five minutes. Answering continues the same sign-in instead of starting
a new one, which would send another new-login email. A wrong PIN goes
straight back to the PIN step and shows how many tries are left. The PIN
goes to the key only and is never stored or logged.

**Two crashes and a hang on the way:**
- **Device-name decoding (SIGABRT).** go-hid converts the device's wide-
  character name with `wcstombs`, which fails on a non-ASCII name in the C
  locale, and go-hid then panicked. Fix: `setlocale(LC_CTYPE, "UTF-8")`
  once before enumerating, plus a recover.
- **hidapi threading (SIGTRAP in CFRunLoopAddSource).** On macOS, hidapi's
  device manager is scheduled on the run loop of the thread that first
  uses it. A Go goroutine moves between OS threads, so the manager ended
  up bound to a thread that had exited. Fix: every enumerate, open and
  close runs on one permanent goroutine locked to its own OS thread.
- **The key never answered (report length).** ctaphid v0.8.3 sent short
  15-byte HID output reports, which the YubiKey ignored, so CTAPHID INIT
  hung. A small C probe showed the key answers a full 64-byte report.
  Fix: ctaphid v0.8.5, which pads packets to 64 bytes. Opens are also
  non-exclusive now, give up after 10 seconds, and one sign-in at a time
  uses the key.

Not supported: passkeys kept in iCloud Keychain, which only Proton's own
apps and website may use. Also only one key may be plugged in at a time.

## What's rclone and what's ours

| Part | Whose |
|---|---|
| Proton Drive file access (listing, upload, download, encryption) | rclone's `protondrive` backend, on Proton-API-Bridge and go-proton-api |
| Google Drive access | rclone's `drive` backend |
| The copy itself (`operations.Copy`), retries, folder cache | rclone |
| Sign-in (password, 2FA code, mailbox password, security key) | ours: `internal/core/proton.go`, `protonkey.go`, `hid_darwin.go` |
| Keeping and encrypting the session | ours, using rclone's config encryption |
| Sync rules, planning, how many files at once | ours: `internal/core/mirror.go` |
| Throttle counting and debug logging | ours, on rclone's HTTP metrics and logger |

rclone is a pinned module dependency and isn't forked. Fixes that would
help everyone (such as per-remote Proton config) belong upstream.

## Sync speed

**Baseline.** The first sync to Proton took about 15 seconds a song. This
was measured on Dean's Mac with a Google Drive primary, 407 FLACs
averaging 31.3 MB (12.7 GB):
- The sync uploaded at 2.1 MB/s (17 Mbps), against an uplink measured at
  116 Mbps (`networkQuality`). That's about 15% of the line.
- CPU was 1 to 7%, so encryption wasn't the cost.
- Traffic came in bursts with idle gaps: download a song from Drive,
  upload it to Proton, wait, then start the next.

**Why one at a time hurt.** Copies to a secondary ran one file after
another. Each Proton upload is several API round trips in series:
- find the parent folder's keys
- create the file draft
- ask for block upload URLs
- upload the encrypted 4 MB blocks
- commit the revision

The uplink's responsiveness under load measured 253 ms (`networkQuality`;
Proton's own round-trip time wasn't measured). The blocks of one file can
only start once the draft exists. One song at a
time, the line sits idle for most of each file. We expect a Google Drive
secondary to suffer less, because Drive takes a file in one resumable
upload with fewer round trips per file and no encryption step. That's
inferred from how the two backends work, not measured.

**The fix.** New files now go to a secondary four at a time
(`errgroup`, limit `fs.GetConfig(ctx).Transfers`, which is rclone's
default of 4):
- This applies to every kind of secondary: Proton, Google Drive and local
  folders.
- rclone's folder cache holds a lock while it finds or creates a folder,
  so two copies into a new folder don't make it twice.
- An upload that is cut off leaves a draft on Proton, which the next try
  replaces (`replace_existing_draft`).

**After.** On the same Mac and library: 8.9 MB/s up (about 61% of the
line), steady with no gaps, and 15 to 16 songs a minute, or about 4
seconds a song instead of 15. Each song still takes about 14 seconds on
its own (2.5 MB/s); the gain is from four running together. No throttling
was seen.

**Why four.** Four is rclone's default and gentle on Proton's rate limits.
The line still had room, so more might be faster. That's an open item,
to try only with the throttle indicator watching.

**Still one at a time.** Replacements (a changed file moved to the trash,
then copied again), removals, and the library's own files still go one
by one. They matter for edits, not for a first copy.

## Throttle instrumentation

Both clouds retry quietly when told to slow down:
- go-proton-api retries 429 and 503 replies after the server's
  `Retry-After`, or after 10 seconds. It writes one NOTICE line when it
  does, but nothing showed this in the app.
- rclone's Drive backend retries through its pacer and logs that only in
  debug mode.

So throttling looked just like a slow sync.

The core now sets rclone's HTTP metrics (`fshttp.DefaultMetrics`, a
counter by host and status). Every Proton and Drive request, retries
included, passes through rclone's transport:
- Every 5 seconds the core logs any rise in 429 and 503 replies for each
  cloud.
- `Status.Throttled` lists the clouds that did this in the last two
  minutes.
- The status bar shows an orange tortoise, "Proton is slowing us down",
  and the tooltip gives the count.

Drive's 403 rate limits aren't counted, because a status code alone
can't tell them from Drive's other 403 refusals.

## Debug logging and the log file

The log goes to stderr and to `~/Library/Logs/MediaLayer.log`, because an
app opened from Finder or the Dock has no stderr that anyone reads. The
file starts again at 10 MB, keeping the previous one as
`MediaLayer.log.1`. The size is checked at launch.

The Library menu has **Debug Logging** (on until turned off) and **Show
Log in Finder**. With debug on, the log also has:
- rclone's info lines
- each copy's time and speed (`copied <path>: 33.6 MB in 14.0s (2.4 MB/s)`)
- each security key step: opened, waiting for the touch to pick, picked,
  PIN accepted or not, waiting for the touch to sign, signed. It never
  includes the PIN.

With debug off, the log keeps notices, warnings and errors, and throttle
lines still appear.

## The overnight crash (go-proton-api bugs)

About 19 minutes into the first fast sync, the app crashed
(EXC_BAD_ACCESS at 0x8, then a Go runtime abort). Here's what happened:

1. A block upload to Proton's storage got "broken pipe".
2. resty retried it with the same `bytes.Reader`, which the first
   attempt had already drained. go-proton-api's `UploadBlock` doesn't
   rewind it, so Proton answered "400 Upload file empty (2003)". A block
   retry can never succeed.
3. That copy failed, and the copy loop cancelled the others.
4. One of them was inside `Client.CreateFile`. That function reads
   `resp.StatusCode()` on the nil response `doRes` returns when no answer
   came (go-proton-api v1.0.4 `link.go:51`; `CreateFolder` has the same
   bug at `:91` and `:94`). The second word of `resty.Response` is at
   offset 8.

This was reproduced in a test. Any network failure during a file or
folder create can trigger it, not only a cancel.

What MediaLayer does about it:
- Every write to a cloud (copy, move to trash, small files, uploads) runs
  under `noPanic`, which turns such a panic into an error. The next sync
  tries again.
- A failed copy no longer cancels the copies under way; they finish, and
  no new ones start.
- A crash's full Go stack now goes to `MediaLayer.log`
  (`runtime/debug.SetCrashOutput`), as stderr is lost for an app opened
  from Finder.

To send upstream (go-proton-api and Proton-API-Bridge, not yet filed):
- **Nil response.** `CreateFile` and `CreateFolder` should check `resp`
  before reading its status, or `doRes` should return the response with
  its error.
- **Empty retry.** `UploadBlock` should give resty a reader it can
  rewind, or turn on `RetryResetReaders`.
- **Leaked goroutines.** In Proton-API-Bridge `uploadPendingBlocks`,
  `errChan` is unbuffered and the function returns on the first error.
  The other block goroutines then block forever and keep their semaphore
  permits. Also, a failed `Acquire` doesn't return, so `Release` is
  called without a permit.

## Open items

- **Proton shares can't be opened.** rclone's backend, on
  Proton-API-Bridge, opens only the account's own main share. So a library
  shared view-only with another Proton user (for example a child's
  account), or through a public link, can't be read. It needs work in
  rclone's backend and in Proton-API-Bridge. Until then, share a Proton
  library through a Google Drive secondary.
- **The official Proton SDK.** rclone builds on Proton-API-Bridge, which
  Proton doesn't maintain. Proton has said it is building an official
  Drive SDK. Whether it is usable from Go, and whether it opens shares,
  hasn't been checked. If it does, it may be the better route for shares
  than patching the bridge.
- **iCloud Keychain passkeys can't sign in.** Only Proton's own apps and
  website may use them. A security key over USB, or an authenticator app
  code, works.
- **More files at once.** The line had about 40% left at four at a time.
  Try 6 or 8 with the throttle indicator on before changing the default.
- **Untested on hardware:** the older-key touch-to-pick path (the dummy
  makeCredential). Dean's FIDO 2.1 key was the one tested.
