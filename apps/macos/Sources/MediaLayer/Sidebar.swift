import SwiftUI

/// The collapsible sidebar: the library's Songs, then the playlists.
/// Drop tracks from the table on a playlist to add them.
struct SidebarView: View {
    @EnvironmentObject var model: AppModel
    @State private var deleting: Playlist?
    @State private var dropTarget: String? // the playlist a drag is over

    // List selection can be cleared; the sidebar always shows something.
    private var selection: Binding<SidebarItem?> {
        Binding(get: { model.sidebarSelection }, set: { if let item = $0 { model.sidebarSelection = item } })
    }

    var body: some View {
        List(selection: selection) {
            Section("Library") {
                Label("Songs", systemImage: "music.note").tag(SidebarItem.songs)
            }
            Section("Playlists") {
                ForEach(model.playlists) { p in
                    PlaylistRow(playlist: p, isDropTarget: dropTarget == p.id)
                        .dropDestination(for: TrackDrag.self) { drops, _ in
                            model.add(TrackDrag.ids(drops), to: p.id)
                            return true
                        } isTargeted: { over in
                            if over { dropTarget = p.id } else if dropTarget == p.id { dropTarget = nil }
                        }
                        .contextMenu {
                            Button("Rename") { model.renamingPlaylist = p.id }
                            Button("Delete Playlist…") { deleting = p }
                            let missing = model.missingCount(p)
                            if missing > 0 {
                                Divider()
                                Button("Remove \(missing) Missing \(missing == 1 ? "Song" : "Songs")") {
                                    model.removeMissing(from: p.id)
                                }
                            }
                        }
                        .tag(SidebarItem.playlist(p.id))
                }
            }
        }
        .listStyle(.sidebar)
        .onDeleteCommand { if let p = model.selectedPlaylist { deleting = p } }
        .safeAreaInset(edge: .bottom, spacing: 0) {
            HStack {
                NewPlaylistButton()
                Spacer()
            }
            .padding(.horizontal, 10)
            .padding(.vertical, 6)
        }
        .confirmationDialog(deleting.map { "Delete the playlist “\($0.name)”?" } ?? "",
                            isPresented: Binding(get: { deleting != nil }, set: { if !$0 { deleting = nil } }),
                            presenting: deleting) { p in
            Button("Delete", role: .destructive) { model.deletePlaylist(p.id) }
        } message: { _ in
            Text("Its songs stay in the library.")
        }
    }
}

/// The + under the sidebar. Apart from SidebarView because it observes the
/// core's status (through showsLibrary), which changes on most polls
/// during a sync; the playlists don't need to redraw then.
private struct NewPlaylistButton: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var core: StatusModel

    var body: some View {
        Button { model.newPlaylist() } label: { Image(systemName: "plus") }
            .buttonStyle(.borderless)
            .help("New Playlist")
            .disabled(!model.showsLibrary)
    }
}

/// A playlist in the sidebar, with its name in a text field while it is
/// being renamed. Return saves the name, Escape keeps the old one, and
/// clicking elsewhere saves it.
struct PlaylistRow: View {
    @EnvironmentObject var model: AppModel
    let playlist: Playlist
    let isDropTarget: Bool
    @State private var name = ""
    @FocusState private var focused: Bool

    private var renaming: Bool { model.renamingPlaylist == playlist.id }

    var body: some View {
        Label {
            if renaming {
                TextField("Playlist Name", text: $name)
                    .textFieldStyle(.plain)
                    .focused($focused)
                    .onSubmit { model.renamePlaylist(playlist.id, to: name) }
                    .onExitCommand { model.renamingPlaylist = nil }
                    .onChange(of: focused) { _, isFocused in
                        if !isFocused, renaming { model.renamePlaylist(playlist.id, to: name) }
                    }
                    .onAppear {
                        name = playlist.name
                        DispatchQueue.main.async { focused = true } // once the field is in the window
                    }
            } else {
                Text(playlist.name).lineLimit(1)
            }
        } icon: {
            Image(systemName: "music.note.list")
        }
        .overlay {
            if isDropTarget {
                RoundedRectangle(cornerRadius: 4, style: .continuous)
                    .strokeBorder(Color.accentColor, lineWidth: 2)
                    .padding(-3)
            }
        }
    }
}

/// File > New Playlist (⌘N) and View > Sort by Playlist Order. The
/// sidebar's Show/Hide Sidebar (⌃⌘S) comes from SidebarCommands.
struct PlaylistCommands: Commands {
    @ObservedObject var model: AppModel
    /// Observed for showsLibrary, which depends on the core's status.
    @ObservedObject var core: StatusModel

    var body: some Commands {
        CommandGroup(replacing: .newItem) {
            Button("New Playlist") { model.newPlaylist() }
                .keyboardShortcut("n")
                .disabled(!model.showsLibrary)
        }
        CommandGroup(after: .sidebar) {
            Button("Sort by Playlist Order") { model.playlistSortOrder = [] }
                .disabled(model.selectedPlaylistID == nil || model.playlistSortOrder.isEmpty)
        }
    }
}
