import AppKit
import CoreTransferable
import Foundation
import SwiftUI
import UniformTypeIdentifiers

/// What the sidebar shows in the table.
enum SidebarItem: Hashable {
    case songs
    case playlist(String) // playlist id
}

/// A playlist as the index holds it. The Go core makes every change; the
/// app reads playlists from the index, as it reads tracks.
struct Playlist: Identifiable, Hashable {
    let id: String
    var name: String
    /// File ids by position, including files that are missing now.
    var items: [String]

    /// Each file's first position. A file the playlist lists twice (which
    /// this app never does) shows once, as the table's rows are tracks.
    var firstPositions: [String: Int] {
        var out: [String: Int] = [:]
        for (pos, file) in items.enumerated() where out[file] == nil { out[file] = pos }
        return out
    }
}

/// Tracks dragged from the table, to a sidebar playlist or within one.
struct TrackDrag: Codable, Transferable, Sendable {
    var ids: [String]
    /// The playlist the rows were dragged from; nil for Songs.
    var playlist: String?

    static var transferRepresentation: some TransferRepresentation {
        CodableRepresentation(contentType: .mediaLayerTracks)
    }

    /// The file ids of a drop, in order, each once: a drag of several rows
    /// can arrive as one item per row, each naming the whole selection.
    static func ids(_ drops: [TrackDrag]) -> [String] {
        var seen = Set<String>()
        return drops.flatMap(\.ids).filter { seen.insert($0).inserted }
    }
}

extension UTType {
    /// Declared in the app's Info.plist (scripts/build-mac-app.sh).
    static let mediaLayerTracks = UTType(exportedAs: "org.medialayer.tracks")
}

extension NSPasteboard.PasteboardType {
    /// TrackDrag's type, for the Songs table's NSTableView drags.
    static let mediaLayerTracks = NSPasteboard.PasteboardType(UTType.mediaLayerTracks.identifier)
}

extension AppModel {
    var selectedPlaylistID: String? {
        if case .playlist(let id) = sidebarSelection { return id }
        return nil
    }

    /// Whether the window shows the library (ContentView's last case), so
    /// playlist commands apply.
    var showsLibrary: Bool {
        startError == nil && status.connected && !status.folder.isEmpty && !choosingFolder && !status.dbPath.isEmpty
    }

    var selectedPlaylist: Playlist? {
        guard let id = selectedPlaylistID else { return nil }
        return playlists.first { $0.id == id }
    }

    /// A playlist's tracks in order. Items whose file is missing from the
    /// library are skipped: they keep their place and come back with it.
    func playlistTracks(_ id: String) -> [Track] {
        guard let p = playlists.first(where: { $0.id == id }) else { return [] }
        var seen = Set<String>()
        return p.items.compactMap { file in
            guard seen.insert(file).inserted else { return nil }
            return trackByID[file]
        }
    }

    /// How many of a playlist's items are hidden because their file is
    /// missing from the library.
    func missingCount(_ p: Playlist) -> Int {
        p.items.reduce(0) { $0 + (trackByID[$1] == nil ? 1 : 0) }
    }

    /// Runs a playlist change in the core, then rereads the playlists and
    /// schedules the sync that saves the change.
    private func change(_ action: () throws -> Void) {
        do { try action() } catch { playlistError = error.localizedDescription }
        do { try reloadPlaylists() } catch { playlistError = error.localizedDescription }
        schedulePublish()
    }

    /// Starts a sync a few seconds after the last playlist change, so the
    /// changes reach the primary and the secondaries soon. A sync that is
    /// running then is waited for: it may have saved the index before the
    /// change.
    func schedulePublish() {
        publishTask?.cancel()
        publishTask = Task { [weak self] in
            try? await Task.sleep(nanoseconds: 5_000_000_000)
            while !Task.isCancelled {
                guard let self else { return }
                if !self.status.isBusy {
                    try? Core.startSync()
                    return
                }
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
        }
    }

    func removeMissing(from id: String) {
        change { try Core.removeMissingFromPlaylist(id) }
    }

    /// Makes a playlist, optionally with tracks, shows it and starts
    /// editing its name.
    func newPlaylist(with fileIDs: [String] = []) {
        do {
            let id = try Core.createPlaylist(name: "Untitled Playlist")
            if !fileIDs.isEmpty { try Core.addToPlaylist(id, fileIDs: fileIDs) }
            try reloadPlaylists()
            sidebarVisibility = .all
            sidebarSelection = .playlist(id)
            renamingPlaylist = id
        } catch {
            playlistError = error.localizedDescription
            try? reloadPlaylists()
        }
        schedulePublish()
    }

    func renamePlaylist(_ id: String, to name: String) {
        if renamingPlaylist == id { renamingPlaylist = nil }
        let name = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !name.isEmpty, name != playlists.first(where: { $0.id == id })?.name else { return }
        change { try Core.renamePlaylist(id, to: name) }
    }

    func deletePlaylist(_ id: String) {
        change { try Core.deletePlaylist(id) }
    }

    /// Adds tracks before a position (-1 for the end). Tracks already in
    /// the playlist are left where they are.
    func add(_ fileIDs: [String], to id: String, before: Int = -1) {
        guard let p = playlists.first(where: { $0.id == id }) else { return }
        var have = Set(p.items)
        let new = fileIDs.filter { have.insert($0).inserted }
        guard !new.isEmpty else { return NSSound.beep() }
        change { try Core.addToPlaylist(id, fileIDs: new, before: before) }
    }

    func remove(_ fileIDs: [String], from id: String) {
        guard let p = playlists.first(where: { $0.id == id }) else { return }
        let first = p.firstPositions
        let positions = fileIDs.compactMap { first[$0] }
        guard !positions.isEmpty else { return }
        change { try Core.removeFromPlaylist(id, positions: positions) }
    }

    /// A drop on the table at row `offset` of `rows`: in a playlist shown in
    /// its own order, rows from it move there and other tracks are added
    /// there. Songs takes no drops.
    func dropOnTable(_ drops: [TrackDrag], at offset: Int, rows: [Track]) {
        guard let p = selectedPlaylist else { return }
        // Sorted or searched: the rows don't show the playlist's order.
        guard playlistSortOrder.isEmpty, !isSearching else { return NSSound.beep() }
        let ids = TrackDrag.ids(drops)
        let first = p.firstPositions
        // Positions count hidden (missing) items too, so name the row the
        // drop lands above by its position.
        let before = rows.indices.contains(offset) ? first[rows[offset].id] ?? -1 : -1
        if drops.allSatisfy({ $0.playlist == p.id }) {
            let positions = ids.compactMap { first[$0] }
            guard !positions.isEmpty else { return }
            change { try Core.movePlaylistItems(p.id, positions: positions, before: before) }
        } else {
            add(ids, to: p.id, before: before)
        }
    }
}
