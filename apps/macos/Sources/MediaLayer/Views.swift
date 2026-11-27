import AppKit
import SwiftUI

@main
struct MediaLayerApp: App {
    // @State, not @StateObject: the scene doesn't observe the model, so a
    // change to it doesn't rebuild the scene and its menus. The menus
    // that show the model's state observe it themselves.
    @State private var model = AppModel()

    var body: some Scene {
        WindowGroup("MediaLayer") {
            ContentView()
                .environmentObject(model)
                .environmentObject(model.statusModel)
                .environmentObject(model.browse)
                .environmentObject(model.player)
                .environmentObject(model.player.clock)
                .frame(minWidth: ToolbarLayout.windowMinWidth, minHeight: 480)
        }
        .commands {
            SidebarCommands()
            PlaylistCommands(model: model, core: model.statusModel)
            LibraryCommands(model: model, core: model.statusModel)
            CommandGroup(after: .toolbar) { ColumnsMenu(columns: model.columns) }
            CommandMenu("Controls") {
                // Space is handled by AppModel's key monitor; a bare-Space
                // menu shortcut could take spaces from text fields.
                Button("Play/Pause") { model.player.toggle() }
                Button("Next") { model.player.next() }
                    .keyboardShortcut(.rightArrow)
                Button("Previous") { model.player.previous() }
                    .keyboardShortcut(.leftArrow)
            }
        }
    }
}

/// Edit > Find and the Library menu, which depend on the core's status.
struct LibraryCommands: Commands {
    @ObservedObject var model: AppModel
    @ObservedObject var core: StatusModel

    var body: some Commands {
        CommandGroup(after: .textEditing) {
            Button("Find") { model.focusSearch() }
                .keyboardShortcut("f")
                .disabled(core.status.folder.isEmpty)
        }
        CommandMenu("Library") {
            Button("Update Library") { model.resync() }
                .keyboardShortcut("r")
                .disabled(core.status.folder.isEmpty || core.status.isBusy)
            Button("Choose Music Folder…") { model.choosingFolder = true }
                .disabled(!core.status.connected || core.status.isBusy)
            Button("Add Source…") { model.addingSource = true }
                .disabled(core.status.folder.isEmpty || core.status.isBusy)
        }
    }
}

struct ContentView: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var core: StatusModel

    var body: some View {
        if let error = model.startError {
            ContentUnavailableView("MediaLayer could not start", systemImage: "exclamationmark.triangle",
                                   description: Text(error))
        } else if !core.status.connected {
            ConnectView()
        } else if core.status.folder.isEmpty || model.choosingFolder {
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

/// Picks the primary music folder.
struct FolderPicker: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        DriveFolderPicker(
            title: "Pick your music folder",
            message: "MediaLayer reads tags and covers from this folder and keeps its index in a hidden .medialayer folder inside it.",
            onCancel: model.choosingFolder ? { model.choosingFolder = false } : nil
        ) { folder in
            try model.useFolder(folder)
            model.choosingFolder = false
        }
    }
}

/// Browses the folders of a connected account and picks one.
struct DriveFolderPicker: View {
    var remote = "" // "" for the primary's account
    /// The name of the top of the drive.
    var rootName = "My Drive"
    let title: String
    let message: String
    var onCancel: (() -> Void)?
    /// Offer to make a new folder inside the current one; MediaLayer
    /// creates it when it first writes there.
    var allowNew = false
    let onPick: (String) throws -> Void

    @State private var newName = ""
    @State private var path: [String] = [] // folder names from the top of the drive
    @State private var folders: [String] = []
    @State private var loading = false
    @State private var error: String?

    private var current: String { path.joined(separator: "/") }
    private var trimmedName: String { newName.trimmingCharacters(in: .whitespacesAndNewlines) }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(title).font(.title)
            Text(message).foregroundStyle(.secondary)
            HStack(spacing: 4) {
                Button(rootName) { path = [] }.buttonStyle(.link)
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
            if allowNew {
                HStack {
                    TextField("New folder in “\(path.last ?? rootName)”", text: $newName)
                        .textFieldStyle(.roundedBorder)
                    Button("Use New Folder") {
                        do { try onPick(path.isEmpty ? trimmedName : current + "/" + trimmedName) }
                        catch { self.error = error.localizedDescription }
                    }
                    .disabled(["", ".", ".."].contains(trimmedName) || trimmedName.contains("/"))
                }
            }
            HStack {
                if let onCancel { Button("Cancel", action: onCancel).keyboardShortcut(.cancelAction) }
                Spacer()
                Button("Use “\(path.last ?? rootName)”") {
                    do { try onPick(current) } catch { self.error = error.localizedDescription }
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
                let list = try await Core.listFolders(remote: remote, in: dir)
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

/// The library window. It observes AppModel only, which changes on user
/// actions and library reloads: the search, the status poll and the
/// playback clock redraw only the views that show them, and the table
/// follows the model by itself (SongsTable.swift).
struct LibraryView: View {
    @EnvironmentObject var model: AppModel
    @State private var selection = Set<Track.ID>()
    /// The detail column's width, which sizes the toolbar items above it.
    /// Seeded with the narrowest case, so the first toolbar pass fits.
    @State private var toolbar = ToolbarLayout(detailWidth: ToolbarLayout.windowMinWidth - ToolbarLayout.sidebarMaxWidth - 12,
                                               sidebarHidden: false)

    var body: some View {
        NavigationSplitView(columnVisibility: $model.sidebarVisibility) {
            SidebarView()
                // ToolbarLayout.windowMinWidth leaves the detail room for the
                // toolbar items with the sidebar at its widest.
                .navigationSplitViewColumnWidth(min: 160, ideal: 200, max: ToolbarLayout.sidebarMaxWidth)
        } detail: {
            VSplitView {
                if model.selectedPlaylistID == nil { // playlists show without the browser, as in iTunes
                    ColumnBrowser()
                        .frame(minHeight: 120, idealHeight: 190, maxHeight: 400)
                        .modifier(NoMinimumWidth())
                }
                // Return plays and Delete removes from a playlist: the
                // table handles both keys.
                SongsTable(model: model, selection: $selection)
                    .frame(minHeight: 200)
                    .modifier(NoMinimumWidth())
                    .onChange(of: selection) { _, ids in
                        // Look up the selected track's cloud file now, so pressing
                        // play doesn't wait for it.
                        if ids.count == 1, let id = ids.first { Core.prepare(Track.fileID(of: id)) }
                    }
            }
            // Nothing wraps the VSplitView itself: a frame around it kept the
            // floating sidebar's safe-area inset from reaching its panes, which
            // then drew under the sidebar.
            .safeAreaInset(edge: .bottom, spacing: 0) {
                // The status bar spans the detail's visible width, beside the
                // sidebar, which is the room the toolbar items share. It
                // starts at the window's edge when the sidebar is hidden,
                // however it was hidden. (Measured on macOS 26. Should an
                // older macOS report the detail's own space here, the items
                // only come out narrower, never into the overflow menu.)
                StatusBar().modifier(NoMinimumWidth())
                    .background(GeometryReader { g in
                        let layout = ToolbarLayout(detailWidth: g.size.width,
                                                   sidebarHidden: g.frame(in: .global).minX < 20)
                        Color.clear.onChange(of: layout, initial: true) { _, t in toolbar = t }
                    })
            }
            // On the detail column, so the items sit in its part of the
            // toolbar, after the sidebar's toggle.
            // The window title would take toolbar room from the transport,
            // now playing and search, which must stay visible.
            .modifier(HiddenWindowTitle())
            .toolbar {
                ToolbarItemGroup(placement: .navigation) { TransportControls() }
                NowPlayingItem(width: toolbar.nowPlayingWidth)
                SearchToolbarItem(width: toolbar.searchWidth)
            }
        }
        .onChange(of: model.sidebarSelection) { selection = [] }
        .onAppear {
            LayoutProbe.startIfRequested {
                model.sidebarVisibility = model.sidebarVisibility == .detailOnly ? .all : .detailOnly
            }
        }
        .sheet(isPresented: $model.addingSource) { AddSourceSheet() }
        // Here, not in the sidebar, so it shows with the sidebar collapsed.
        .alert("Couldn't change the playlist",
               isPresented: Binding(get: { model.playlistError != nil }, set: { if !$0 { model.playlistError = nil } })) {
        } message: { Text(model.playlistError ?? "") }
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
struct NowPlayingDisplay: View {
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
                    MarqueeText(text: t.displayTitle, font: .system(size: 12, weight: .medium), height: 15)
                    MarqueeText(text: [t.artist, t.album].filter { !$0.isEmpty }.joined(separator: " — "),
                                font: .system(size: 11), height: 14)
                        .foregroundStyle(.secondary)
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

/// The toolbar item for NowPlayingDisplay. On macOS 26 the toolbar would wrap it
/// in its own capsule; hide that, since NowPlayingDisplay draws its own shape.
struct NowPlayingItem: ToolbarContent {
    let width: CGFloat
    var body: some ToolbarContent {
        if #available(macOS 26.0, *) {
            ToolbarItem(placement: .principal) { item }.sharedBackgroundVisibility(.hidden)
        } else {
            ToolbarItem(placement: .principal) { item }
        }
    }

    // A fixed width from ToolbarLayout: the toolbar sizes an item by its
    // ideal width and hides items that don't fit, so a flexible frame here
    // pushed the transport and search into the overflow menu. Text that
    // doesn't fit scrolls (MarqueeText).
    private var item: some View { NowPlayingDisplay().frame(width: width) }
}

/// Widths for the toolbar items over the detail column. The toolbar
/// doesn't shrink SwiftUI items; it hides items that don't fit in its
/// overflow menu, search first, then the transport. So the now-playing
/// display and search take fixed widths that leave room for all of them
/// at every window width.
struct ToolbarLayout: Equatable {
    /// The sidebar's widest. The split view draws it a few points wider.
    static let sidebarMaxWidth: CGFloat = 260
    /// Room for the transport (3 × ~40), the narrowest search and now
    /// playing, and the toolbar's spacing, beside the widest sidebar.
    static let windowMinWidth: CGFloat = 840
    static let transportWidth: CGFloat = 125
    static let spacing: CGFloat = 100 // item spacing and the toolbar's margins
    /// With the sidebar hidden, the window buttons and the sidebar toggle
    /// move into the detail's part of the toolbar.
    static let chromeWidth: CGFloat = 130

    let nowPlayingWidth: CGFloat
    let searchWidth: CGFloat

    init(detailWidth: CGFloat, sidebarHidden: Bool) {
        // Steps of 8 points, so a window resize redraws the toolbar now
        // and then, not every frame.
        let d = (detailWidth / 8).rounded(.down) * 8
        let room = d - Self.transportWidth - Self.spacing - (sidebarHidden ? Self.chromeWidth : 0)
        // Now playing grows first; search widens only once it is full width.
        searchWidth = min(200, max(150, room - 460))
        nowPlayingWidth = min(460, max(180, room - searchWidth))
    }
}

/// Lets a view's frame be narrower than its contents' minimum. Without it
/// the split view takes the contents' minimum, plus the floating sidebar's
/// inset counted at each level, as its own: narrower than that, it laid
/// out wider than the window and centred itself, cutting off the sidebar on
/// the left and the table on the right. The window's minimum leaves the
/// contents their own minimum width, so nothing needs clipping.
struct NoMinimumWidth: ViewModifier {
    func body(content: Content) -> some View {
        content.frame(minWidth: 0, maxWidth: .infinity)
    }
}

struct ColumnBrowser: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var browse: BrowseSignal

    var body: some View {
        // Equatable, so a column whose values and choice are unchanged
        // isn't rebuilt when another column changes.
        HStack(spacing: 0) {
            BrowserColumn(title: "Genres", values: model.genres, selection: $model.genre).equatable()
            columnDivider
            BrowserColumn(title: "Composers", values: model.composers, selection: $model.composer).equatable()
            columnDivider
            BrowserColumn(title: "Artists", values: model.artists, selection: $model.artist).equatable()
            columnDivider
            BrowserColumn(title: "Albums", values: model.albums, selection: $model.album).equatable()
        }
    }

    /// Dividers start below the header row: one that met the toolbar's
    /// bottom edge read as a stray tick under the now-playing display.
    private var columnDivider: some View {
        Divider().padding(.top, BrowserColumn.headerHeight)
    }
}

struct BrowserColumn: View, Equatable {
    static let headerHeight: CGFloat = 22
    let title: String
    let values: [String]
    @Binding var selection: String?
    /// The choice when this view was made. A Binding reads the live value,
    /// so == compares this instead.
    let selected: String?

    init(title: String, values: [String], selection: Binding<String?>) {
        self.title = title
        self.values = values
        _selection = selection
        selected = selection.wrappedValue
    }

    static func == (a: BrowserColumn, b: BrowserColumn) -> Bool {
        a.title == b.title && a.selected == b.selected && a.values == b.values
    }

    var body: some View {
        VStack(spacing: 0) {
            Text(title).font(.caption.bold()).foregroundStyle(.secondary)
                .frame(maxWidth: .infinity).frame(height: Self.headerHeight)
            Divider()
            BrowserList(title: title, values: values, selection: $selection)
        }
        .frame(minWidth: 90)
    }
}

/// A browser column's list: "All (n Genres)", then the values. An
/// NSTableView with reused AppKit cells: a SwiftUI List made a hosting
/// view per row, and rebuilt them all when the values changed.
struct BrowserList: NSViewRepresentable {
    let title: String
    let values: [String]
    @Binding var selection: String?

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeNSView(context: Context) -> NSScrollView { context.coordinator.scrollView }

    func updateNSView(_ view: NSScrollView, context: Context) {
        context.coordinator.selection = $selection
        context.coordinator.show(title: title, values: values, selected: selection)
    }

    @MainActor
    final class Coordinator: NSObject, NSTableViewDataSource, NSTableViewDelegate {
        static let cellID = NSUserInterfaceItemIdentifier("BrowserList.cell")

        let scrollView = NSScrollView()
        var selection: Binding<String?>?
        private let table = NSTableView()
        private var title = ""
        private var values: [String] = []
        /// Each value's row; row 0 is All.
        private var rowOf: [String: Int] = [:]
        /// Set while this code changes the table, so its selection
        /// callbacks don't write back to the model.
        private var updating = false

        override init() {
            super.init()
            let column = NSTableColumn(identifier: NSUserInterfaceItemIdentifier("value"))
            column.minWidth = 40
            column.width = 120
            column.resizingMask = .autoresizingMask
            table.addTableColumn(column)
            table.headerView = nil
            table.style = .plain
            table.usesAutomaticRowHeights = false
            table.rowHeight = 20
            table.columnAutoresizingStyle = .lastColumnOnlyAutoresizingStyle
            table.allowsMultipleSelection = false
            table.allowsEmptySelection = false
            table.allowsTypeSelect = true
            table.dataSource = self
            table.delegate = self
            scrollView.documentView = table
            scrollView.hasVerticalScroller = true
            scrollView.autohidesScrollers = true
            scrollView.borderType = .noBorder
        }

        /// Reloads only when the values change, then selects the model's
        /// choice, or All.
        func show(title: String, values: [String], selected: String?) {
            if title != self.title || values != self.values {
                self.title = title
                self.values = values
                var rows = [String: Int](minimumCapacity: values.count)
                for (i, v) in values.enumerated() where rows[v] == nil { rows[v] = i + 1 }
                rowOf = rows
                updating = true
                table.reloadData()
                updating = false
            }
            let row = selected.flatMap { rowOf[$0] } ?? 0
            if table.selectedRow != row {
                updating = true
                table.selectRowIndexes(IndexSet(integer: row), byExtendingSelection: false)
                updating = false
            }
        }

        func numberOfRows(in tableView: NSTableView) -> Int { values.count + 1 }

        func tableView(_ tableView: NSTableView, viewFor tableColumn: NSTableColumn?, row: Int) -> NSView? {
            let cell = (tableView.makeView(withIdentifier: Self.cellID, owner: nil) as? TextCell)
                ?? TextCell(identifier: Self.cellID, numeric: false)
            cell.textField?.stringValue = text(row)
            return cell
        }

        private func text(_ row: Int) -> String {
            if row == 0 { return "All (\(values.count) \(values.count == 1 ? String(title.dropLast()) : title))" }
            return values.indices.contains(row - 1) ? values[row - 1] : ""
        }

        func tableView(_ tableView: NSTableView, typeSelectStringFor tableColumn: NSTableColumn?, row: Int) -> String? {
            row == 0 ? nil : text(row)
        }

        func tableViewSelectionDidChange(_ notification: Notification) {
            guard !updating, table.selectedRow >= 0 else { return }
            let row = table.selectedRow
            let new: String? = row == 0 || !values.indices.contains(row - 1) ? nil : values[row - 1]
            if selection?.wrappedValue != new { selection?.wrappedValue = new }
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
            let loaded = await model.art.image(sha, size: px)
            if !Task.isCancelled { image = loaded } // not a cover since replaced
        }
    }
}

struct StatusBar: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var core: StatusModel
    @EnvironmentObject var browse: BrowseSignal // the summary follows the search

    var body: some View {
        let s = model.visibleSummary
        HStack(spacing: 10) {
            // Chips keep their names on one line and the job text gives way. Many
            // sources could still crowd out the rest of a narrow bar.
            SourcesBar().fixedSize()
            jobText.foregroundStyle(.secondary).lineLimit(1)
            if let n = core.status.notice, !core.status.isBusy {
                Image(systemName: "exclamationmark.circle").foregroundStyle(.orange).help(n)
            }
            Spacer()
            Text("\(s.count) \(s.count == 1 ? "song" : "songs"), \(formatLong(s.durationMS)), \(ByteCountFormatter.string(fromByteCount: s.bytes, countStyle: .file))")
                .lineLimit(1).truncationMode(.middle)
                .layoutPriority(1) // over the job text, so a narrow bar keeps the counts
            Spacer()
            StartLatency()
        }
        .font(.caption)
        // Clear of the window's rounded bottom corners (large on macOS 26).
        .padding(.horizontal, 22)
        .padding(.vertical, 8)
        .background(.bar)
    }

    /// The primary's cloud, as the status line names it.
    private var primaryCloud: String {
        let type = core.status.sources?.first { $0.primary }?.type ?? "drive"
        return type == "local" ? "this Mac" : SourceIcon.label(type)
    }

    @ViewBuilder private var jobText: some View {
        let j = core.status.job
        if let e = model.libraryError ?? core.status.error {
            Text("Can't read the library: \(e)").foregroundStyle(.red).help(e)
        } else {
            switch j.phase {
            case "opening": Text("Opening library from \(primaryCloud)…")
            case "listing": Text("Listing \(core.status.folder)… \(j.done) files")
            case "indexing": Text("Indexing \(j.done) of \(j.total)" + (j.failed > 0 ? ", \(j.failed) failed" : ""))
            case "publishing": Text("Saving the library to \(primaryCloud)…")
            case "promoting": Text("Switching the primary…")
            case "waiting": Text("Syncing on \(j.holder ?? "another device"). This Mac will sync after it.")
            case "mirroring":
                let s = core.status.sources?.first { $0.state == "syncing" }
                Text("Copying to \(s?.name ?? "secondaries")…" + (j.total > 0 ? " \(j.done) of \(j.total)" : ""))
            case "error": Text("Update failed: \(j.error ?? "")").foregroundStyle(.red).help(j.error ?? "")
            case "done" where j.failed > 0:
                Text("\(j.failed) files could not be read").help((j.failures ?? []).joined(separator: "\n"))
            default: EmptyView()
            }
        }
    }
}

/// How long the last play took to start. Apart from StatusBar because it
/// observes the playback clock, which ticks four times a second.
struct StartLatency: View {
    @EnvironmentObject var clock: PlaybackClock

    var body: some View {
        if let l = clock.startLatency {
            Text(String(format: "started in %.1f s", l)).foregroundStyle(.secondary)
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

// MARK: - Sources

/// The library's folders, in the status bar: an icon for each folder's
/// cloud, the primary highlighted, and the state of each secondary.
struct SourcesBar: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var core: StatusModel
    @State private var approving: CoreStatus.Source?
    @State private var promoting: CoreStatus.Source?
    @State private var error: String?

    var body: some View {
        HStack(spacing: 4) {
            ForEach(core.status.sources ?? []) { source in
                chip(source)
            }
            Button { model.addingSource = true } label: {
                Image(systemName: "plus").font(.caption.weight(.semibold))
            }
            .buttonStyle(.borderless)
            .foregroundStyle(.secondary)
            .disabled(core.status.isBusy)
            .help("Add a folder that keeps a copy of the library")
        }
        .confirmationDialog(approving.map { "Apply changes to “\($0.name)”?" } ?? "",
                            isPresented: Binding(get: { approving != nil }, set: { if !$0 { approving = nil } }),
                            presenting: approving) { source in
            Button("Apply Changes", role: .destructive) {
                do { try model.approveChanges(source.id) } catch { self.error = error.localizedDescription }
            }
        } message: { source in
            Text(approvalMessage(source))
        }
        .confirmationDialog(promoting.map { "Make “\($0.name)” the primary?" } ?? "",
                            isPresented: Binding(get: { promoting != nil }, set: { if !$0 { promoting = nil } }),
                            presenting: promoting) { source in
            Button("Make Primary") {
                do { try model.promote(source.id) } catch { self.error = error.localizedDescription }
            }
        } message: { source in
            Text("“\(source.name)” becomes the library's source of truth, and the current primary becomes a copy that follows it. Nothing is moved: the copy already holds every file. You can switch back the same way once the old primary shows as synced.")
        }
        .alert("Couldn't change the source", isPresented: Binding(get: { error != nil }, set: { if !$0 { error = nil } })) {
        } message: { Text(error ?? "") }
    }

    private func chip(_ source: CoreStatus.Source) -> some View {
        HStack(spacing: 4) {
            SourceIcon(type: source.type)
            Text(source.name).fontWeight(source.primary ? .semibold : .regular)
            if !source.primary { badge(source) }
        }
        .padding(.horizontal, 7)
        .padding(.vertical, 2)
        .foregroundStyle(source.primary ? AnyShapeStyle(.tint) : AnyShapeStyle(.secondary))
        .background {
            if source.primary { Capsule().fill(.tint.opacity(0.15)) }
        }
        .contentShape(Capsule())
        .help(help(source))
        .contextMenu {
            if source.state == "paused" {
                Button("Review Changes…") { approving = source }
            }
            if !source.primary {
                Button("Make Primary…") { promoting = source }
                    .disabled(source.state != "synced" || core.status.isBusy)
                Button("Stop Syncing to “\(source.name)”") {
                    do { try model.removeSecondary(source.id) } catch { self.error = error.localizedDescription }
                }
                .disabled(core.status.isBusy)
            }
        }
        .onTapGesture { if source.state == "paused" { approving = source } }
    }

    @ViewBuilder private func badge(_ source: CoreStatus.Source) -> some View {
        switch source.state {
        case "syncing": ProgressView().controlSize(.mini)
        case "paused": Image(systemName: "pause.circle.fill").foregroundStyle(.orange)
        case "error": Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.red)
        case "synced": Image(systemName: "checkmark").font(.caption2).foregroundStyle(.tertiary)
        default: EmptyView()
        }
    }

    private func help(_ s: CoreStatus.Source) -> String {
        let place = "\(SourceIcon.label(s.type)): \(s.folder)"
        if s.primary { return "Primary, the library's source of truth. \(place)" }
        switch s.state {
        case "paused": return "Paused: \(approvalMessage(s)) Click to review. \(place)"
        case "error": return "Couldn't sync: \(s.error ?? ""). \(place)"
        case "syncing": return "Copying from the primary. \(place)"
        case "synced": return "Copy of the primary, up to date. \(place)"
        default: return "Copy of the primary. \(place)"
        }
    }

    private func approvalMessage(_ s: CoreStatus.Source) -> String {
        var parts: [String] = []
        if s.deletes > 0 { parts.append("move \(s.deletes) files that aren’t on the primary (including any added to this folder directly) to its trash") }
        if s.replaces > 0 {
            let retagged = (s.tagOnly ?? 0) > 0 ? " (\(s.tagOnly ?? 0) only retagged)" : ""
            parts.append("replace \(s.replaces) files that differ\(retagged), keeping the old ones in its trash")
        }
        if s.copies > 0 { parts.append("copy \(s.copies) new files") }
        var message = "This sync would " + parts.joined(separator: ", ") + "."
        if let limit = s.limit {
            let recent = s.recent ?? 0
            message += recent > 0
                ? " \(recent) files were already removed or replaced here in the last day, and a day allows \(limit) without asking."
                : " A day allows \(limit) without asking."
        }
        return message + " The trash keeps files for 30 days."
    }
}

/// An icon for a source's cloud.
struct SourceIcon: View {
    let type: String
    var size: CGFloat = 12

    var body: some View {
        if type == "drive" {
            DriveGlyph().stroke(style: StrokeStyle(lineWidth: 1.3, lineJoin: .round))
                .frame(width: size, height: size * 0.87)
        } else {
            Image(systemName: Self.symbol(type)).font(.system(size: size * 0.95))
        }
    }

    static func symbol(_ type: String) -> String {
        switch type {
        case "local": "externaldrive"
        case "iclouddrive": "icloud"
        case "protondrive": "lock.icloud"
        case "dropbox": "shippingbox"
        case "smb": "server.rack"
        default: "cloud"
        }
    }

    static func label(_ type: String) -> String {
        switch type {
        case "drive": "Google Drive"
        case "local": "Folder on this Mac"
        case "iclouddrive": "iCloud Drive"
        case "protondrive": "Proton Drive"
        case "dropbox": "Dropbox"
        case "smb": "Network share"
        default: type
        }
    }
}

/// A plain triangle of three bands, standing for Google Drive.
struct DriveGlyph: Shape {
    func path(in r: CGRect) -> Path {
        var p = Path()
        let top = CGPoint(x: r.midX, y: r.minY)
        let left = CGPoint(x: r.minX, y: r.maxY)
        let right = CGPoint(x: r.maxX, y: r.maxY)
        p.move(to: top)
        p.addLine(to: right)
        p.addLine(to: left)
        p.closeSubpath()
        // The inner triangle that splits it into three bands.
        let a = CGPoint(x: r.minX + r.width * 0.25, y: r.midY)
        let b = CGPoint(x: r.maxX - r.width * 0.25, y: r.midY)
        let c = CGPoint(x: r.midX, y: r.maxY)
        p.move(to: a)
        p.addLine(to: b)
        p.addLine(to: c)
        p.closeSubpath()
        return p
    }
}

/// Adds a secondary: a folder on a Google or Proton account or on this
/// Mac, which then gets a copy of the library and follows the primary.
struct AddSourceSheet: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var core: StatusModel
    @Environment(\.dismiss) private var dismiss
    @State private var account: String? // the remote whose folders to browse
    /// Signing in to Proton, with this username filled in.
    @State private var protonSignIn: String?
    /// The account being browsed is a Proton one.
    @State private var browsingProton = false
    @State private var working = false
    @State private var error: String?

    var body: some View {
        if let account {
            let proton = browsingProton || (core.status.protonAccounts?.contains { $0.remote == account } ?? false)
            DriveFolderPicker(
                remote: account,
                rootName: proton ? "Proton Drive" : "My Drive",
                title: "Pick the folder for the copy",
                message: "MediaLayer copies the library here and keeps it in step with the primary, one way. Files it removes or replaces go to the folder's .medialayer/trash for 30 days.",
                onCancel: { self.account = nil; browsingProton = false },
                allowNew: true
            ) { folder in
                try model.addSecondary(remote: account, folder: folder)
                dismiss()
            }
            .frame(width: 560, height: 480)
        } else if let user = protonSignIn {
            ProtonSignInView(user: user, onCancel: { protonSignIn = nil }) { remote in
                protonSignIn = nil
                browsingProton = true
                account = remote
            }
            .frame(width: 460)
        } else {
            chooser.frame(width: 460)
        }
    }

    private var primaryRemote: String? { core.status.sources?.first { $0.primary }?.remote }

    private var chooser: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Add a source").font(.title2)
            Text("A second source holds a full copy of your library. The primary stays the source of truth, and every sync makes the copy match it.")
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            ForEach(core.status.accounts ?? [], id: \.self) { remote in
                option(icon: "drive",
                       title: remote == primaryRemote ? "Google Drive, same account" : "Google Drive (\(remote))",
                       detail: remote == primaryRemote ? "Another folder in the primary's Google account" : "A folder in that Google account") {
                    account = remote
                }
            }
            option(icon: "drive", title: "Google Drive, another account", detail: "Your browser opens to sign in") {
                working = true
                error = nil
                Task {
                    do { account = try await Core.addGoogleAccount() } catch { self.error = error.localizedDescription }
                    working = false
                }
            }
            .disabled(working)
            ForEach(core.status.protonAccounts ?? [], id: \.self) { a in
                option(icon: "protondrive", title: "Proton Drive (\(a.user))",
                       detail: a.signedIn ? "A folder in that Proton account" : "Signed out: sign in again to use it") {
                    if a.signedIn {
                        browsingProton = true
                        account = a.remote
                    } else {
                        protonSignIn = a.user
                    }
                }
            }
            if let a = core.status.protonAccounts?.first {
                option(icon: "protondrive", title: "Proton Drive, sign in again",
                       detail: "If its folders can't be opened, or to switch to another Proton account") {
                    protonSignIn = a.user
                }
            } else {
                option(icon: "protondrive", title: "Proton Drive", detail: "Sign in with your Proton account") {
                    protonSignIn = ""
                }
            }
            option(icon: "local", title: "A folder on this Mac or a disk", detail: "Such as an external drive") {
                pickLocal()
            }
            if working { Text("Waiting for Google…").foregroundStyle(.secondary) }
            if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
            }
        }
        .padding(24)
    }

    private func option(icon: String, title: String, detail: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(spacing: 12) {
                SourceIcon(type: icon, size: 20).frame(width: 24)
                VStack(alignment: .leading, spacing: 2) {
                    Text(title)
                    Text(detail).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                Image(systemName: "chevron.right").foregroundStyle(.tertiary)
            }
            .padding(10)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
    }

    private func pickLocal() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.prompt = "Use Folder"
        panel.message = "Pick an empty folder, or one that already holds a copy of this library."
        guard panel.runModal() == .OK, let url = panel.url else { return }
        do {
            try model.addSecondary(remote: "", folder: url.path)
            dismiss()
        } catch { self.error = error.localizedDescription }
    }
}

/// Signs in to Proton Drive. The password is sent to Proton and not
/// kept: MediaLayer keeps only the session. When Proton ends it, the
/// sync's error says so, and the user signs in again here from Add
/// Source. A 2FA code or a mailbox password is asked for when the account
/// needs one.
struct ProtonSignInView: View {
    @State var user: String
    let onCancel: () -> Void
    let onSignedIn: (String) -> Void

    /// The sign-in's steps. A security key's PIN has a step of its own,
    /// with no Return shortcut, and the touch a screen with no fields at
    /// all: a YubiKey touched early types a one-time code and presses
    /// Return, which must never land in a field or press a button.
    private enum Step { case account, pin }

    @State private var step = Step.account
    @State private var password = ""
    @State private var mailbox = ""
    @State private var code = ""
    @State private var askCode = false
    @State private var askMailbox = false
    @State private var pin = "" // the security key's; cleared after each try
    @State private var codeInstead = false // the account has a 2FA code too
    @State private var working = false
    @State private var touchKey = Core.KeyTouch.idle
    @State private var error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Sign in to Proton Drive").font(.title2)
            if touchKey != .idle {
                touchScreen
            } else if step == .pin {
                pinStep
            } else {
                accountStep
            }
            if working && touchKey == .idle {
                Text("Signing in…").foregroundStyle(.secondary)
            }
            if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
            HStack {
                Button("Cancel") {
                    if working { Core.cancelProtonSignIn() }
                    onCancel()
                }.keyboardShortcut(.cancelAction)
                Spacer()
                if touchKey == .idle { primaryButton }
            }
        }
        .padding(24)
        .frame(minWidth: 460)
        .onDisappear { if working { Core.cancelProtonSignIn() } }
        // A sign-in with a security key waits for its touch.
        .task(id: working) {
            while working && !Task.isCancelled {
                touchKey = Core.protonKeyWaiting
                try? await Task.sleep(nanoseconds: 250_000_000)
            }
            touchKey = .idle
        }
    }

    private var accountStep: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Your password goes to Proton only. MediaLayer keeps the session Proton gives it, which unlocks your files until Proton ends it. One Proton account can be connected at a time.")
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            Form {
                TextField("Username or email", text: $user)
                    .textContentType(.username)
                SecureField("Password", text: $password)
                    .textContentType(.password)
                if askMailbox {
                    SecureField("Mailbox password", text: $mailbox)
                }
                if askCode {
                    TextField("2FA code", text: $code, prompt: Text("From your authenticator app"))
                        .textContentType(.oneTimeCode)
                }
            }
            .disabled(working)
        }
    }

    private var pinStep: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Your security key has a PIN. Type it here and click Continue. Don't touch the key yet: MediaLayer will tell you when.")
                .fixedSize(horizontal: false, vertical: true)
            Form {
                LabeledContent("Username", value: user)
                SecureField("Security key PIN", text: $pin)
                    .textContentType(.oneTimeCode) // not a password to save
                    .onSubmit {} // Return does nothing here
            }
            .disabled(working)
            if codeInstead {
                Button("Use a code from your authenticator app instead") {
                    pin = ""
                    error = nil
                    askCode = true
                    step = .account
                }
                .buttonStyle(.link)
                .disabled(working)
            }
        }
    }

    private var touchScreen: some View {
        VStack(spacing: 12) {
            Image(systemName: "key.fill").font(.system(size: 40))
            if touchKey == .toPick {
                Text("Touch your security key").font(.title3.bold())
                Text("Touch the key while it blinks, to use it for this sign-in. Then you'll enter its PIN.")
                    .foregroundStyle(.secondary).multilineTextAlignment(.center)
            } else {
                Text("Touch your security key to sign in").font(.title3.bold())
                Text("Touch the key while it blinks, to finish signing in.")
                    .foregroundStyle(.secondary).multilineTextAlignment(.center)
            }
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 24)
    }

    @ViewBuilder private var primaryButton: some View {
        switch step {
        case .account:
            Button("Sign In", action: signIn)
                .buttonStyle(.borderedProminent)
                .keyboardShortcut(.defaultAction)
                .disabled(working || user.trimmingCharacters(in: .whitespaces).isEmpty || password.isEmpty
                          || (askMailbox && mailbox.isEmpty))
        case .pin:
            // No Return shortcut: only a click sends the PIN.
            Button("Continue", action: sendPIN)
                .buttonStyle(.borderedProminent)
                .disabled(working || pin.isEmpty)
        }
    }

    /// Checks the PIN before it goes to the key, where a wrong one costs
    /// one of the key's few tries.
    private func sendPIN() {
        if let problem = Core.securityKeyPINProblem(pin) {
            pin = ""
            error = problem
            return
        }
        signIn()
    }

    private func signIn() {
        working = true
        error = nil
        let pinNow = step == .pin ? pin : ""
        Task {
            do {
                let remote = try await Core.connectProton(user: user, password: password, mailbox: mailbox, code: code, pin: pinNow)
                pin = ""
                working = false
                onSignedIn(remote)
            } catch let e as Core.ProtonSignInError {
                working = false
                error = e.message
                code = "" // each try signs in afresh, and a code is good once
                pin = ""
                switch e.need {
                case .twoFA: askCode = true; step = .account
                case .mailbox: askMailbox = true; step = .account
                case .pin:
                    codeInstead = e.or == .twoFA
                    if step != .pin { error = nil } // the step says it
                    step = .pin
                case nil: break
                }
            } catch {
                working = false
                code = ""
                pin = ""
                self.error = error.localizedDescription
            }
        }
    }
}

/// Hides the window title in the toolbar, keeping it for the Window menu
/// and Mission Control where the OS allows.
struct HiddenWindowTitle: ViewModifier {
    func body(content: Content) -> some View {
        if #available(macOS 15.0, *) {
            content.toolbar(removing: .title)
        } else {
            content.navigationTitle("")
        }
    }
}

/// One line of text, centred. When it is wider than its room it scrolls
/// slowly to its end and back, pausing at each end, as in iTunes' display.
/// Text that fits doesn't move, and each scroll is a finite animation, so
/// nothing animates between them.
struct MarqueeText: View {
    let text: String
    let font: Font
    let height: CGFloat
    @State private var textWidth: CGFloat = 0
    @State private var atEnd = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        GeometryReader { g in
            let overflow = max(0, textWidth - g.size.width)
            Text(text).font(font).lineLimit(1).fixedSize()
                .background(GeometryReader { t in
                    Color.clear
                        .onAppear { textWidth = t.size.width }
                        .onChange(of: t.size.width) { _, w in textWidth = w }
                })
                .offset(x: overflow > 4 && atEnd ? -overflow : 0)
                .frame(width: g.size.width, alignment: overflow > 4 ? .leading : .center)
                // Restarts when the text or its room changes.
                .task(id: Int(overflow)) {
                    atEnd = false
                    guard overflow > 4, !reduceMotion else { return }
                    let leg = Double(overflow) / 25
                    while !Task.isCancelled {
                        try? await Task.sleep(nanoseconds: 2_000_000_000)
                        if Task.isCancelled { return }
                        withAnimation(.linear(duration: leg)) { atEnd.toggle() }
                        try? await Task.sleep(nanoseconds: UInt64(leg * 1_000_000_000))
                    }
                }
        }
        .frame(height: height)
        .clipped()
        .id(text) // a new track starts again from the left
    }
}
