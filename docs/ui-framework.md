# UI framework: stay native or pivot?

Written November 2026, while the Mac player feels laggy (jumpy resize,
search and sidebar) with the 407-track test library. The question: is
SwiftUI the wrong choice for an app that targets macOS, Windows and Linux,
then iOS and Android, and should MediaLayer move to a fast cross-platform
UI toolkit such as Rust's gpui?

**Decided (26 November 2026): stay native and fix the table** (path A below).

## Recommendation

**Don't pivot. Keep the Apple apps native and fix the Songs table, which
is the lag.** Decide the non-Apple UI at the Windows milestone, with
Flutter as the leading candidate for Windows, Linux and Android together.

1. The lag is SwiftUI's `Table`, not native UI. 407 rows is tiny for any
   toolkit; `NSTableView` scrolls 100,000 rows smoothly. Milestone 3
   already says the track table may need `NSTableView`.
2. The core is already UI-neutral (see below), so a second UI later costs
   only the UI, not the core. Nothing needs to change now to keep that
   option open.
3. Apple platforms are the first two targets, and the features planned
   for them (Now Playing, media keys, menu bar control, the mount, Sparkle,
   iOS locked-screen playback) are first class in Swift and are plugins
   or hand-written bridges in every cross-platform toolkit.
4. The fastest toolkits (gpui, Slint) are not yet a safe base for five
   platforms: gpui is pre-1.0 with no official mobile support, and
   Slint's iOS port is a young, Rust-only preview.

## Why the Mac app lags

These are SwiftUI `Table` costs, so the profiling thread should confirm
them before any fix:

- `SongsTable` builds every row as a `TableRow` with `.draggable`, and each
  shown column (up to 16, 10 by default) builds a SwiftUI view per visible
  cell. SwiftUI's `Table`
  diffs all rows when the `tracks` array changes, so a keystroke in search
  or a column-browser click redoes that work. Apple's forums have many
  reports of `Table` hanging with large row counts, with regressions in
  macOS 15.5 ([784845](https://developer.apple.com/forums/thread/784845),
  [765653](https://developer.apple.com/forums/thread/765653)).
- `AppModel` is one `ObservableObject` with many `@Published` properties.
  Any change re-evaluates every view that observes it, the table included.
  The table also observes `Player`, so a change to `isPlaying` or `error`
  re-evaluates it too.
- Live resize of the window or sidebar makes `Table` lay out every shown
  column through SwiftUI on every frame.

The fix stays in Swift: an `NSTableView` behind `NSViewRepresentable` for
the Songs table (cell reuse, its own sorting, drag and column
customisation), and splitting `AppModel` (and the table's use of `Player`)
so the table observes only what it shows. The column browser can stay SwiftUI or move to `NSTableView`
too. Gate: milestone 3's "a 10,000-track table scrolls smoothly".

## What the core boundary already gives us

| Boundary | How the Mac app uses it | Any other UI |
| --- | --- | --- |
| Commands | `cmd/mlcore`: about 20 C functions that take and return JSON strings (`-buildmode=c-archive`) | Any language with a C FFI: Swift, Rust, Dart, C#, C++, Kotlin/Native; JVM through JNA or Panama |
| Library data | Reads the SQLite index directly (WAL, read-only) | Every toolkit has SQLite |
| Playback | `AVPlayer` on the core's localhost stream | Any player that takes an HTTP URL: libmpv, Media3, Windows' MediaPlayer, GStreamer |

So the Go-binding cost is roughly the same small layer for every
candidate. The Go-native toolkits avoid it entirely, but it is already
written and small.

## The candidates in November 2026

Ratings are for this app: a big sortable table, five platforms, background
audio and media keys, and a Go core.

| Toolkit | Language | Speed for big tables | Platforms | Media integration | Verdict |
| --- | --- | --- | --- | --- | --- |
| **SwiftUI + AppKit/UIKit** | Swift | Fast with `NSTableView`/`UITableView`; SwiftUI `Table` alone is slow | Apple only | Native: Now Playing, remote commands, background audio | **Keep for Apple** |
| **Flutter** | Dart | Good; lazy 2D table views (`two_dimensional_scrollables`) | All five, desktop maintained by Canonical since May 2026 | Mature plugins: `audio_service`, `media_kit` (libmpv), SMTC on Windows, MPRIS on Linux | **Best single UI for Windows, Linux and Android** |
| Compose Multiplatform | Kotlin | Good (lazy layouts) | All five; iOS stable since 1.8 (May 2025); desktop runs on the JVM | Android native (Media3); others hand-written | Strong for Android; JVM desktop and two binding paths to Go |
| Wails v3 / Tauri 2 | Go or Rust + web UI | Good with a virtualised table; WebKitGTK is slow on Linux | Wails v3 is out, with mobile experimental and outside its stability promise; Tauri 2 has mobile | Web Media Session only, so native bridges needed | Wails is the cheapest Windows/Linux app (no FFI at all), but weak on mobile |
| gpui | Rust | Fastest (GPU, built for Zed) | macOS, Windows, Linux; iOS backend still a pull request, Android only in community forks; pre-1.0 (0.2.x on crates.io) | None; all hand-written | Too early to bet five platforms on |
| Slint | Rust/C++ + `.slint` | Fast (GPU or software) | Desktop and Android; iOS since 1.12 as a tech preview, Rust only | None built in | Promising, small widget set; GPLv3, royalty-free with attribution, or commercial |
| Qt 6 / QML | C++ | Fast | All five | Qt Multimedia; media sessions partly hand-written | Capable, but C++ and LGPL limits on iOS |
| Avalonia | C# | Good, virtualised DataGrid | Desktop strong, mobile supported | None built in | Fine desktop choice, weaker mobile story |
| Gio | Go | Fast (immediate mode) | All five, via `gogio` | None; no table widget, little accessibility | Go-native but too much to hand-build |
| Fyne | Go | Weak for wide sortable tables | All five | None | Not suited to an iTunes-style table |

## Paths compared

| Path | UI codebases | Cost now | Risk |
| --- | --- | --- | --- |
| **A. Apple native, one cross-platform UI for the rest (recommended)** | 2 (SwiftUI for Mac/iOS; Flutter or Wails for the rest) | Days: swap the table | Low. The second UI is thin because the core does the work |
| B. Flutter everywhere, now | 1 | Rewrite the ~2,900 lines of Swift; Now Playing, mount and menu bar through plugins | Medium. Less Mac-like, and the lag fix does not need it |
| C. gpui (Rust) | 1 desktop, mobile unknown | Rewrite, plus a Rust↔Go bridge and all media integration | High. Pre-1.0, mobile unofficial |
| D. Go-native (Gio, Fyne, Wails) | 1 desktop, mobile experimental or weak | Rewrite; table and media integration hand-built | Medium–high for mobile |

Path B is a fair alternative if one UI codebase matters more than a Mac
app that feels like Music.app. Its argument is that the Swift is still
small, so if a pivot is coming it is cheapest now. Against it: the Mac and
iOS are the first two targets, the lag has a known native fix, and the
non-Apple UI can be chosen later at no extra cost because the core is
already behind a C interface.

## What changes in the code

For path A, only `apps/macos`: the Songs table moves to `NSTableView` and
`AppModel` is split. The core, `cmd/mlcore` and the index stay as they
are. At the Windows milestone, the new UI reuses `cmd/mlcore` (built with
`-buildmode=c-shared` for Dart FFI, or called directly from Go with Wails)
and the "leaning toward Wails" line in [milestones.md](milestones.md) is
replaced with the choice made then. If that choice is Flutter, it also
replaces milestones.md's Android plan (Kotlin with Jetpack Compose and a
gomobile AAR) with Dart FFI on the `c-shared` core.

## Sources

- SwiftUI Table performance: [Apple forum 784845](https://developer.apple.com/forums/thread/784845), [765653](https://developer.apple.com/forums/thread/765653), [NSTableView with SwiftUI](https://mjtsai.com/blog/2024/04/15/nstableview-with-swiftui)
- Flutter: [2026 roadmap](https://flutter.dev/blog/flutter-darts-2026-roadmap), [Canonical leads Flutter desktop](https://www.omgubuntu.co.uk/2026/05/flutter-desktop-canonical-maintained)
- Compose Multiplatform: [1.8.0, iOS stable](https://blog.jetbrains.com/kotlin/2025/05/compose-multiplatform-1-8-0-released-compose-multiplatform-for-ios-is-stable-and-production-ready/)
- Wails: [v3.0.0 release](https://github.com/wailsapp/wails/releases)
- gpui: [iOS backend PR](https://github.com/zed-industries/zed/pull/63068), [crates.io release discussion](https://github.com/zed-industries/zed/discussions/55271), [gpui-mobile fork](https://github.com/longbridge/gpui-mobile)
- Slint: [1.17](https://slint.dev/blog/slint-1.17-released), [iOS guide](https://docs.slint.dev/latest/docs/slint/guide/platforms/mobile/ios/)
