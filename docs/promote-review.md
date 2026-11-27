# Make Primary: where the time goes, and what to change

A review of sync and Make Primary for speed and robustness, written after
six switches on Dean's Mac on 2026-11-27. The library had 496 files and
three replicas: two Google Drive folders and one Proton Drive folder.
Build 50f99d6.

**Done since (2026-11-27):** items 1 (f64fd3a), 2 (5af7857), 4 (c2ca909),
5 without reading repo.json once (05051e6), and 10 (a96450d). The rest is
still the plan to choose from.

## Measurements

All six jobs ended ok and none copied a file, so every second below is
overhead.

| Job | Total | Before the switch | Switch | After the switch |
|---|---|---|---|---|
| Google to Proton | 2m45 | open 4.7, list 2.4, mirror 62.4 | 16.6 | list 17.2, publish 8.7, mirror 53.3 |
| Proton to Google | 3m44 | open 28.9, list 20.4, mirror 25.9 | 11.3 | list 1.3, **index 51.9**, publish 2.5, mirror 81.8 |
| Google to Google | 2m30 | open 4.4, list 1.6, publish 3.4, mirror 67.1 | 5.1 | list 2.2, mirror 65.8 |

- **Listing:** Google 1.3–2.7 s, Proton 9–15 s as a secondary and 16–20 s
  as the primary.
- **Opening:** a Proton primary takes 27–29 s to open (Google 4–6 s).
- **Small writes to Proton:** each logs a 422 "already exists" and takes
  6–12 s. A job does 3–8 of them.

## Why it is slow

These figures come from reading the code and counting API calls, not from
measuring them:

- **A Proton listing is about three requests per folder where one would
  do.** The per-file revision details come with the folder listing and
  are only decrypted locally. The cost is a `GetLink` for each folder
  (child links aren't cached) and paging until an empty page (always two
  requests) (Proton-API-Bridge `folder.go`, `cache.go`; go-proton-api
  `link_folder.go`).
- **Overwriting a small file on Proton is about 13–14 requests in a
  row:**
  1. a lookup
  2. `CreateFile`, which fails with the 422
  3. a second search
  4. list revisions
  5. create a revision
  6. request a block upload
  7. upload the block
  8. commit

  Reading a small file is about 6 requests.
- **Opening a Proton primary:** about 3–5 s of that is the new client,
  2 s is reading `repo.json`, then 14–26 s goes on taking the sync lease
  (list, write `waiting`, list, write `held`), and 2 s on reading
  `repo.json` again.
- **The phase names hide lease costs.** "Opening" includes taking the
  lease, and the mirroring phases include taking and releasing the
  target's.

## Redundant work

| What | Verdict |
|---|---|
| **Re-indexing after Proton to Google** (45–52 s) | **A bug.** The switch to Proton rewrites every file's time to Proton's whole second. On the way back, Drive's times have milliseconds, so every file looks changed. Every file is re-read, and its audio hash is cleared and computed again (`indexer.go:104-112`, `withinPrecision`). |
| The primary is listed twice per sync (`indexer.go:68`, `mirror.go:502`) | Reuse the indexer's listing, except when a probe was newly written this run. |
| The target is listed as a secondary, then again as the new primary | Reuse it when the sync before the switch changed nothing on it. |
| The probe file is rewritten on every sync (`mirror.go:614`) | Write it only when it's missing or different. The check needs a known value, not a fresh one. |
| `member.json` is written twice per secondary (`copying`, then `synced`) | Skip the `copying` write when there is nothing to copy, replace or remove. |
| `repo.json` is read 4 times per sync, 8 per Make Primary | Read it once after the lease is held. |
| A fresh Proton client for each job, plus a second one during a switch | Use one client per remote and folder per job. This also avoids two clients on one session fighting over the refresh token. |
| The full sync after the switch when nothing changed | Could be a sync of only the records (repo.json and member.json). That changes how long the old primary's strict review covers, so it's a decision. |

## Dean's ideas

**Local caching.**
- **A cache kept between runs: not worth it.**
  - A cache of revision details would save only local decryption, a
    second or two, because the details already come with the listing.
  - A saved listing can't be checked cheaply against Proton, which offers
    no change feed for Drive in the library. A stale listing would drive
    removal decisions.
- **Reusing listings and clients within a job: yes.** This is the biggest
  safe win (items 4, 5 and 7 below).

**A checkout of the database, renewed so it doesn't expire.** This is
worse than the current per-job sync lease as a default.
- **What it saves:** taking and releasing the lease, which costs 18–36 s
  per job on a Proton primary.
- **What it costs:**
  - Every other device is locked out while the app is open.
  - On Proton, each renewal is a 12–20 s burst of requests and a new
    revision of the lock file, about 720 a day.
  - More cases to handle around sleep and a lapsed checkout.
- **It doesn't make the local database more trustworthy.** Files change
  outside MediaLayer whatever lock is held.
- **Better:** take the lease with one write instead of two (item 6). Or
  keep it for a couple of minutes after a job so back-to-back jobs reuse
  it, though that only helps a session of repeated switches.

**A version id that changes on every write.** Worth doing for
correctness. It saves no time. Today the numbers miss these cases:

- **Same number, different index.** Make Primary trusts that the target
  holds version N because member.json says N (`promote.go:277`). If two
  devices each made a version N while the lease failed, the target could
  hold the other device's index, and this device would never publish its
  own.
- **The primary changes behind the device's back.** Publish skips
  checking the primary when the local index is clean (`repo.go:383`). A
  primary restored or rolled back, or another device's newer snapshot,
  goes unnoticed until the index next changes.
- **A local database restored from a backup.** Say it is version 40 while
  the primary holds 42. It publishes as 43 and overwrites the playlists
  made in 41 and 42.

The smallest design changes no file formats:
- **Stored:** a new random id (and its parent's id) with each version, in
  the index's meta table, so it travels inside every snapshot. Also
  `primary_version_id` beside `primary_version`, and `version_id` in
  member.json.
- **Checked:**
  - At publish, list `snapshots/` and require that the newest is the
    version this device last published, with a matching hash. If it
    isn't, take the primary's version and index on top of it, as restore
    already does.
  - In Make Primary, require the ids to match.

It costs one small listing per sync. Do it before a second device is used
in earnest.

## Robustness gaps

1. **Two Proton clients on one session during a switch.** If the token
   renews in that window, one client is refused for the rest of the job.
   If that's the lease's client, the job dies or the lock lingers for 10
   minutes. Item 5 fixes this.
2. **`ReviewFirst` holds one id.** If the old primary's strict review
   didn't finish and another folder is then promoted, the first loses
   its strict review. It should be a set.
3. **Files added to the old primary after a switch.** Another device or
   a desktop uploader still writing to the old folder will see those
   files trashed by the next ordinary sync, up to the day's limit,
   without asking. They can be restored from trash for 30 days.
4. **A wrong clock.** A device whose clock is a month ahead empties every
   trash folder, and drops the day's limit count early.
5. **The lease's release has a 30 s timeout.** A slow Proton overwrite
   can exceed it and leave the lock held for 10 minutes. Otherwise the
   lease timing is sound: a renewal of 12–20 s against a 9-minute lapse.
6. **Two devices with the lock at once** are only noticed at the next
   renewal, up to 2 minutes in. The version id catches what they wrote.
7. The switch's own crash points are covered (by the recovery in 50f99d6).

## Recommendations

| # | Change | Saves | Risk | Size |
|---|---|---|---|---|
| 1 | Stop retiming; compare times at the coarser cloud's precision | ~60 s on Proton to Google, ~12 s on Google to Proton | Low | ~20 lines |
| 2 | One member.json write when no files change | 6–12 s per Proton secondary per sync | Low | ~10 lines |
| 3 | Write the probe only when missing or different | 5–10 s per Proton secondary per sync | Low | ~25 lines |
| 4 | List the primary once per sync | 16–20 s per sync on a Proton primary | Low to medium | ~70 lines |
| 5 | One client per remote and folder per job; read repo.json once | 3–8 s, and closes gap 1 | Low | ~40 lines |
| 6 | Take the lease with one write | 6–12 s per lease on Proton | Medium (it's the lock) | ~40 lines |
| 7 | Sync only the records after a switch that changed nothing | 40–60 s per switch | Medium, and a decision | ~120 lines |
| 8 | Run each secondary's checks concurrently | 10–15 s per sync with a Proton secondary | Low to medium | ~40 lines |
| 9 | Upstream: cache child links and stop paging on a short page; overwrite by going straight to a new revision | Proton listing 2–3x faster; overwrites ~14 to ~8 requests | None to us | Small patches |
| 10 | Version id | none | Low | ~120 lines |
| 11 | `ReviewFirst` as a set, a guard against a wrong clock, a longer release timeout on Proton, log `SetMeta` errors | none | Low | Small |
| 12 | Log the lease and each secondary's time separately | none (measurement) | None | ~10 lines |

The estimates below are inferred:
- Items 1–5 should bring a switch from 2.5–3.7 minutes to about 1.5–2.
- Adding items 6–8 should bring it to about 1–1.5.
- The floor is Proton's time per request, which only the upstream fixes
  (item 9) move.
