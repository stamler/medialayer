import SwiftUI

@main
struct MediaLayerApp: App {
    @StateObject private var model = AppModel()

    var body: some Scene {
        WindowGroup("MediaLayer") {
            ContentView()
                .environmentObject(model)
                .environmentObject(model.player)
                .environmentObject(model.player.clock)
                .frame(minWidth: 900, minHeight: 560)
        }
        .commands {
            CommandMenu("Library") {
                Button("Update Library") { model.resync() }
                    .keyboardShortcut("r")
                    .disabled(model.status.folder.isEmpty || model.status.isBusy)
                Button("Choose Music Folder…") { model.choosingFolder = true }
                    .disabled(!model.status.connected || model.status.isBusy)
            }
            CommandMenu("Controls") {
                Button("Play/Pause") { model.player.toggle() }
                    .keyboardShortcut(.space, modifiers: [])
                Button("Next") { model.player.next() }
                    .keyboardShortcut(.rightArrow)
                Button("Previous") { model.player.previous() }
                    .keyboardShortcut(.leftArrow)
            }
        }
    }
}

struct ContentView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        if let error = model.startError {
            ContentUnavailableView("MediaLayer could not start", systemImage: "exclamationmark.triangle",
                                   description: Text(error))
        } else if !model.status.connected {
            ConnectView()
        } else if model.status.folder.isEmpty || model.choosingFolder {
            FolderPicker()
        } else {
            LibraryView()
        }
    }
}

// MARK: - Setup

struct ConnectView: View {
    @EnvironmentObject var model: AppModel
    @State private var working = false
    @State private var error: String?

    var body: some View {
        VStack(spacing: 16) {
            Image(systemName: "music.note.house").font(.system(size: 56)).foregroundStyle(.secondary)
            Text("Welcome to MediaLayer").font(.largeTitle)
            Text("Connect the cloud that holds your music. Your browser opens to sign in.")
                .foregroundStyle(.secondary)
            Button {
                working = true
                error = nil
                Task {
                    do { try await model.connect() } catch { self.error = error.localizedDescription }
                    working = false
                }
            } label: {
                Label(working ? "Waiting for Google…" : "Connect Google Drive", systemImage: "icloud")
                    .frame(minWidth: 220)
            }
            .controlSize(.large)
            .buttonStyle(.borderedProminent)
            .disabled(working)
            if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
        }
        .padding(40)
    }
}

struct FolderPicker: View {
    @EnvironmentObject var model: AppModel
    @State private var path: [String] = [] // folder names from the top of My Drive
    @State private var folders: [String] = []
    @State private var loading = false
    @State private var error: String?

    private var current: String { path.joined(separator: "/") }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Pick your music folder").font(.title)
            Text("MediaLayer reads tags and covers from this folder and keeps its index in a hidden .medialayer folder inside it.")
                .foregroundStyle(.secondary)
            HStack(spacing: 4) {
                Button("My Drive") { path = [] }.buttonStyle(.link)
                ForEach(path.indices, id: \.self) { i in
                    Image(systemName: "chevron.right").foregroundStyle(.tertiary)
                    Button(path[i]) { path = Array(path[...i]) }.buttonStyle(.link)
                }
            }
            List(folders, id: \.self) { full in
                Button {
                    path = full.split(separator: "/").map(String.init)
                } label: {
                    Label((full as NSString).lastPathComponent, systemImage: "folder")
                }
                .buttonStyle(.plain)
            }
            .overlay { if loading { ProgressView() } }
            if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
            HStack {
                if model.choosingFolder {
                    Button("Cancel") { model.choosingFolder = false }
                }
                Spacer()
                Button("Use “\(path.last ?? "My Drive")”") {
                    do {
                        try model.useFolder(current)
                        model.choosingFolder = false
                    } catch { self.error = error.localizedDescription }
                }
                .buttonStyle(.borderedProminent)
                .disabled(path.isEmpty)
            }
        }
        .padding(24)
        .frame(maxWidth: 640)
        .task(id: current) {
            loading = true
            error = nil
            let dir = current
            do {
                let list = try await Core.listFolders(in: dir)
                guard !Task.isCancelled else { return } // the user moved on
                folders = list
            } catch {
                guard !Task.isCancelled else { return }
                folders = []
                self.error = error.localizedDescription
            }
            loading = false
        }
    }
}

// MARK: - Library

struct LibraryView: View {
    @EnvironmentObject var model: AppModel
    @State private var selection = Set<Track.ID>()

    var body: some View {
        let visible = model.visibleTracks
        VSplitView {
            ColumnBrowser()
                .frame(minHeight: 120, idealHeight: 190, maxHeight: 400)
            SongsTable(tracks: visible, selection: $selection)
                .frame(minHeight: 200)
                .onChange(of: selection) { _, ids in
                    // Look up the selected track's cloud file now, so pressing
                    // play doesn't wait for it.
                    if ids.count == 1, let id = ids.first { Core.prepare(id) }
                }
        }
        .safeAreaInset(edge: .bottom, spacing: 0) { StatusBar(tracks: visible) }
        .toolbar {
            ToolbarItemGroup(placement: .navigation) { TransportControls() }
            NowPlayingItem()
        }
    }
}

struct TransportControls: View {
    @EnvironmentObject var player: Player
    var body: some View {
        Button { player.previous() } label: { Image(systemName: "backward.fill") }
        Button { player.toggle() } label: {
            Image(systemName: player.isPlaying ? "pause.fill" : "play.fill")
        }
        Button { player.next() } label: { Image(systemName: "forward.fill") }
    }
}

/// The now-playing display in the toolbar: a cover framed by a squarer left
/// end, then the title, artist, album and progress, inside a shape whose
/// right end is a full capsule curve.
struct NowPlaying: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var player: Player
    @EnvironmentObject var clock: PlaybackClock

    static let height: CGFloat = 48
    static let inset: CGFloat = 4 // around the cover, on all sides
    static let coverRadius: CGFloat = 5
    /// The left corners are concentric with the cover's.
    static let leftRadius = coverRadius + inset

    static var shape: UnevenRoundedRectangle {
        UnevenRoundedRectangle(topLeadingRadius: leftRadius, bottomLeadingRadius: leftRadius,
                               bottomTrailingRadius: height / 2, topTrailingRadius: height / 2,
                               style: .continuous)
    }

    var body: some View {
        HStack(spacing: 10) {
            ArtView(sha: player.current?.artSHA, size: Self.height - 2 * Self.inset,
                    cornerRadius: Self.coverRadius)
            VStack(spacing: 1) {
                if let t = player.current {
                    Text(t.displayTitle).font(.system(size: 12, weight: .medium)).lineLimit(1)
                    Text([t.artist, t.album].filter { !$0.isEmpty }.joined(separator: " — "))
                        .font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(1)
                    Scrubber(duration: player.duration, position: clock.position) { player.seek(to: $0) }
                        .padding(.top, 2)
                } else {
                    Text("MediaLayer").font(.system(size: 12)).foregroundStyle(.secondary)
                }
            }
            .frame(maxWidth: .infinity)
            if let e = player.error {
                Image(systemName: "exclamationmark.triangle").foregroundStyle(.red).help(e)
            }
        }
        .padding(.leading, Self.inset)
        .padding(.trailing, Self.height / 2) // clear of the right end's curve
        .frame(height: Self.height)
        .modifier(TransportBackground(shape: Self.shape))
    }
}

/// The playback position as a thin line between elapsed and remaining
/// time. Click or drag anywhere on it to seek; the seek happens when the
/// mouse is released, so dragging doesn't start a cloud request per step.
struct Scrubber: View {
    let duration: Double // seconds
    let position: Double
    let seek: (Double) -> Void
    // Resets by itself if the drag is cancelled (the window loses focus).
    @GestureState private var dragFraction: Double?

    private var fraction: Double {
        dragFraction ?? (duration > 0 ? min(max(position / duration, 0), 1) : 0)
    }

    var body: some View {
        HStack(spacing: 6) {
            Text(clock(fraction * duration)).frame(width: long ? 44 : 34, alignment: .trailing)
            GeometryReader { g in
                ZStack(alignment: .leading) {
                    Capsule().fill(.quaternary).frame(height: 3)
                    Capsule().fill(.secondary).frame(width: g.size.width * fraction, height: 3)
                    Circle().fill(.primary)
                        .frame(width: 7, height: 7)
                        .offset(x: g.size.width * fraction - 3.5)
                        .opacity(dragFraction == nil ? 0 : 1)
                }
                .frame(maxHeight: .infinity)
                .contentShape(Rectangle())
                .gesture(DragGesture(minimumDistance: 0)
                    .updating($dragFraction) { v, state, _ in
                        guard duration > 0, g.size.width > 0 else { return }
                        state = min(max(v.location.x / g.size.width, 0), 1)
                    }
                    .onEnded { v in
                        guard duration > 0, g.size.width > 0 else { return }
                        let f = min(max(v.location.x / g.size.width, 0), 1)
                        // Not the very end: the tagged length can run past the
                        // audio, and seeking there would skip the track.
                        seek(min(f * duration, max(duration - 0.5, 0)))
                    })
            }
            .frame(height: 10)
            Text("-" + clock(max(duration - fraction * duration, 0)))
                .frame(width: long ? 48 : 38, alignment: .leading)
        }
        .font(.system(size: 9).monospacedDigit())
        .foregroundStyle(.secondary)
    }

    private var long: Bool { duration >= 3600 } // h:mm:ss needs more room

    private func clock(_ seconds: Double) -> String { formatDuration(Int64(seconds * 1000)) }
}

/// Liquid Glass on macOS 26, a material before it.
struct TransportBackground<S: InsettableShape>: ViewModifier {
    let shape: S

    func body(content: Content) -> some View {
        if #available(macOS 26.0, *) {
            content.glassEffect(.regular, in: shape)
        } else {
            content.background(.regularMaterial, in: shape)
                .overlay(shape.strokeBorder(.separator, lineWidth: 0.5))
        }
    }
}

/// The toolbar item for NowPlaying. On macOS 26 the toolbar would wrap it
/// in its own capsule; hide that, since NowPlaying draws its own shape.
struct NowPlayingItem: ToolbarContent {
    var body: some ToolbarContent {
        if #available(macOS 26.0, *) {
            ToolbarItem(placement: .principal) { item }.sharedBackgroundVisibility(.hidden)
        } else {
            ToolbarItem(placement: .principal) { item }
        }
    }

    private var item: some View { NowPlaying().frame(minWidth: 360, idealWidth: 460) }
}

struct ColumnBrowser: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        HStack(spacing: 0) {
            BrowserColumn(title: "Genres", values: model.genres, selection: $model.genre)
            Divider()
            BrowserColumn(title: "Composers", values: model.composers, selection: $model.composer)
            Divider()
            BrowserColumn(title: "Artists", values: model.artists, selection: $model.artist)
            Divider()
            BrowserColumn(title: "Albums", values: model.albums, selection: $model.album)
        }
    }
}

struct BrowserColumn: View {
    let title: String
    let values: [String]
    @Binding var selection: String?

    // A change waiting to be applied (see listSelection).
    @State private var pending: String??

    // List selection can't hold nil, so "All" is the empty string.
    private var listSelection: Binding<String?> {
        Binding(get: { (pending ?? selection) ?? "" }, set: { value in
            let new = (value ?? "").isEmpty ? nil : value
            guard new != (pending ?? selection) else { return }
            // The list can report a selection change while the table is
            // updating (when indexing changes the values). Changing the model
            // inside that callback is reentrant for AppKit, so do it after.
            pending = .some(new)
            DispatchQueue.main.async {
                if let p = pending { selection = p }
                pending = nil
            }
        })
    }

    var body: some View {
        VStack(spacing: 0) {
            Text(title).font(.caption.bold()).foregroundStyle(.secondary)
                .frame(maxWidth: .infinity).padding(.vertical, 4)
            Divider()
            List(selection: listSelection) {
                Text("All (\(values.count) \(values.count == 1 ? String(title.dropLast()) : title))").tag("")
                ForEach(values, id: \.self) { Text($0).lineLimit(1).tag($0) }
            }
            .listStyle(.plain)
        }
        .frame(minWidth: 140)
    }
}

struct SongsTable: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var player: Player
    let tracks: [Track]
    @Binding var selection: Set<Track.ID>

    var body: some View {
        Table(tracks, selection: $selection, sortOrder: $model.sortOrder) {
            TableColumn("") { t in
                ZStack {
                    ArtView(sha: t.artSHA, size: 18)
                    if player.current?.id == t.id {
                        Image(systemName: "speaker.wave.2.fill").font(.caption2)
                            .foregroundStyle(.white).shadow(radius: 2)
                    }
                }
            }
            .width(24)
            TableColumn("Name", value: \.displayTitle)
                .width(min: 120, ideal: 240)
            TableColumn("Time", value: \.durationMS) { t in
                Text(formatDuration(t.durationMS)).monospacedDigit()
            }
            .width(50)
            TableColumn("Artist", value: \.artist).width(min: 80, ideal: 160)
            TableColumn("Album", value: \.album).width(min: 80, ideal: 180)
            TableColumn("Genre", value: \.genre).width(min: 60, ideal: 90)
            TableColumn("Year", value: \.year).width(44)
            TableColumn("#", value: \.trackNumber) { t in
                Text(t.trackNumber > 0 ? "\(t.trackNumber)" : "").monospacedDigit()
            }
            .width(30)
            TableColumn("Kind", value: \.format) { t in Text(kind(t)) }.width(min: 60, ideal: 90)
        }
        .contextMenu(forSelectionType: Track.ID.self) { ids in
            if let id = ids.first, let t = tracks.first(where: { $0.id == id }) {
                Button("Play") { model.play(t) }
            }
        } primaryAction: { ids in
            if let id = ids.first, let t = tracks.first(where: { $0.id == id }) { model.play(t) }
        }
    }

    private func kind(_ t: Track) -> String {
        switch t.format {
        case "flac" where t.bitDepth > 0 && t.sampleRate > 0:
            return "FLAC \(t.bitDepth)/\(t.sampleRate / 1000)"
        case "mp3": return "MP3"
        default: return t.format.uppercased()
        }
    }
}

struct ArtView: View {
    @EnvironmentObject var model: AppModel
    let sha: String?
    let size: CGFloat
    var cornerRadius: CGFloat = 2
    @State private var image: NSImage?

    var body: some View {
        Group {
            if let image {
                Image(nsImage: image).resizable().aspectRatio(contentMode: .fill)
            } else {
                Rectangle().fill(.quaternary)
                    .overlay(Image(systemName: "music.note").font(.system(size: size * 0.45)).foregroundStyle(.tertiary))
            }
        }
        .frame(width: size, height: size)
        .clipShape(RoundedRectangle(cornerRadius: cornerRadius, style: .continuous))
        .task(id: sha) {
            guard let sha else { image = nil; return }
            let px = Int(size * 2) // Retina
            if let hit = model.art.cached(sha, size: px) { image = hit; return }
            image = nil
            let loaded = await withCheckedContinuation { done in
                model.art.load(sha, size: px) { done.resume(returning: $0) }
            }
            if !Task.isCancelled { image = loaded } // not a cover since replaced
        }
    }
}

struct StatusBar: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var clock: PlaybackClock
    let tracks: [Track]

    var body: some View {
        let ms = tracks.reduce(Int64(0)) { $0 + $1.durationMS }
        let bytes = tracks.reduce(Int64(0)) { $0 + $1.size }
        HStack {
            jobText.foregroundStyle(.secondary)
            Spacer()
            Text("\(tracks.count) \(tracks.count == 1 ? "song" : "songs"), \(formatLong(ms)), \(ByteCountFormatter.string(fromByteCount: bytes, countStyle: .file))")
            Spacer()
            if let l = clock.startLatency {
                Text(String(format: "started in %.1f s", l)).foregroundStyle(.secondary)
            }
        }
        .font(.caption)
        // Clear of the window's rounded bottom corners (large on macOS 26).
        .padding(.horizontal, 22)
        .padding(.vertical, 8)
        .background(.bar)
    }

    @ViewBuilder private var jobText: some View {
        let j = model.status.job
        if let e = model.libraryError ?? model.status.error {
            Text("Can't read the library: \(e)").foregroundStyle(.red).help(e)
        } else {
            switch j.phase {
            case "opening": Text("Opening library from Google Drive…")
            case "listing": Text("Listing \(model.status.folder)… \(j.done) files")
            case "indexing": Text("Indexing \(j.done) of \(j.total)" + (j.failed > 0 ? ", \(j.failed) failed" : ""))
            case "publishing": Text("Saving the library to Google Drive…")
            case "error": Text("Update failed: \(j.error ?? "")").foregroundStyle(.red).help(j.error ?? "")
            case "done" where j.failed > 0:
                Text("\(j.failed) files could not be read").help((j.failures ?? []).joined(separator: "\n"))
            default: Text(model.status.folder)
            }
        }
    }
}

func formatDuration(_ ms: Int64) -> String {
    let s = ms / 1000
    return s >= 3600 ? String(format: "%d:%02d:%02d", s / 3600, s / 60 % 60, s % 60)
        : String(format: "%d:%02d", s / 60, s % 60)
}

func formatLong(_ ms: Int64) -> String {
    let minutes = ms / 60_000
    if minutes < 60 { return "\(minutes) minutes" }
    let hours = Double(minutes) / 60
    return hours < 48 ? String(format: "%.1f hours", hours) : String(format: "%.1f days", hours / 24)
}
