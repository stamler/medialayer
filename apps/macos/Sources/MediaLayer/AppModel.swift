import AppKit
import AVFoundation
import Foundation
import SwiftUI

/// The core's status, polled every half second. It is apart from AppModel
/// so that a poll redraws only the views that show it: the status bar, the
/// source chips and the menus. During a sync it changes on most polls.
@MainActor
final class StatusModel: ObservableObject {
    @Published var status = CoreStatus()
}

/// The app's state: the library, the column browser's selection, the
/// playlists and the player. The core's status is in `statusModel`.
@MainActor
final class AppModel: ObservableObject {
    let statusModel = StatusModel()
    /// The core's status as last polled. Views that show it observe
    /// `statusModel`; this model doesn't publish when it changes.
    var status: CoreStatus { statusModel.status }
    @Published private(set) var startError: String?
    /// The last failure reading the index; cleared by the next good read.
    @Published private(set) var libraryError: String?
    @Published private(set) var tracks: [Track] = [] {
        didSet {
            trackByID = Dictionary(tracks.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
            search.invalidate()
            searched = search.filter(tracks, query: searchText)
        }
    }
    /// The library's present tracks by file id, for playlists.
    private(set) var trackByID: [String: Track] = [:]
    /// The toolbar's search text; the column browser and table show only
    /// the tracks it leaves.
    @Published var searchText = "" {
        didSet { if searchText != oldValue { applySearch() } }
    }
    /// Bumped by Edit > Find; the search field focuses itself on a change.
    @Published private(set) var searchFocusRequest = 0
    /// The tracks the search leaves: all of them when it is empty.
    @Published private(set) var searched: [Track] = [] {
        didSet {
            searchedIDs = isSearching ? Set(searched.map(\.id)) : nil
            searchedGeneration += 1
        }
    }
    /// Raised each time `searched` is set, which every change to the tracks
    /// or the search does: part of visibleTracks's cache key.
    private var searchedGeneration = 0
    /// The ids of `searched` while a search is on, for playlists; nil when
    /// the search is empty.
    private(set) var searchedIDs: Set<String>?
    /// Whether the search text has any word to search for.
    var isSearching: Bool { !TrackSearch.words(searchText).isEmpty }
    /// What the Clouds column needs from the index (see Clouds.swift).
    @Published private(set) var cloudIndex = CloudIndex()
    @Published var choosingFolder = false

    // Column browser: nil means "All".
    @Published var genre: String? { didSet { if genre != oldValue { composer = nil; artist = nil; album = nil } } }
    @Published var composer: String? { didSet { if composer != oldValue { artist = nil; album = nil } } }
    @Published var artist: String? { didSet { if artist != oldValue { album = nil } } }
    @Published var album: String?
    @Published var librarySortOrder = [KeyPathComparator(\Track.browseArtist), KeyPathComparator(\Track.album),
                                       KeyPathComparator(\Track.discNumber), KeyPathComparator(\Track.trackNumber)]
    /// A playlist's sort; empty means playlist order, the only order in
    /// which its rows can be dragged to reorder them.
    @Published var playlistSortOrder: [KeyPathComparator<Track>] = []
    /// The table's sort: the library's, or the playlist's when one is shown.
    var sortOrder: [KeyPathComparator<Track>] {
        get { selectedPlaylistID == nil ? librarySortOrder : playlistSortOrder }
        set { if selectedPlaylistID == nil { librarySortOrder = newValue } else { playlistSortOrder = newValue } }
    }

    // Sidebar and playlists (see Playlists.swift).
    @Published var sidebarVisibility = NavigationSplitViewVisibility.all
    @Published var sidebarSelection = SidebarItem.songs {
        didSet {
            guard sidebarSelection != oldValue else { return }
            playlistSortOrder = []
            // A rename left open on another playlist ends, unsaved.
            if let r = renamingPlaylist, sidebarSelection != .playlist(r) { renamingPlaylist = nil }
        }
    }
    @Published var playlists: [Playlist] = [] {
        didSet { playlistsGeneration += 1 }
    }
    private var playlistsGeneration = 0
    /// visibleTracks's last result, and what it was computed from.
    private var visibleCache: (key: VisibleKey, tracks: [Track])?
    /// Debounces the sync that publishes playlist changes (Playlists.swift).
    var publishTask: Task<Void, Never>?
    /// The playlist whose name is being edited in the sidebar.
    @Published var renamingPlaylist: String?
    @Published var playlistError: String?

    let art = ArtCache()
    private let search = TrackSearch()
    let player = Player()
    /// Control Centre's Now Playing and the media keys.
    private var nowPlaying: NowPlaying?

    private var db: LibraryDB?
    private var loadedEpoch: Int64 = -1
    private var loadedCount: Int64 = -1
    private var lastLoad = Date.distantPast
    private var loadedFinish: String?
    private var timer: Timer?
    private var keyMonitor: Any?

    init() {
        nowPlaying = NowPlaying(player: player, art: art)
        let dir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("MediaLayer", isDirectory: true)
        do {
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            try Core.start(dataDir: dir)
        } catch {
            startError = error.localizedDescription
            return
        }
        refresh()
        if !status.folder.isEmpty, status.connected {
            try? Core.startSync() // pick up changes made elsewhere since the last run
        }
        let timer = Timer(timeInterval: 0.5, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.refresh() }
        }
        RunLoop.main.add(timer, forMode: .common) // keep polling while menus are open
        self.timer = timer
        // Space plays and pauses anywhere in the library window. A menu
        // shortcut with no modifier never sees it: the table and lists
        // take it first. Text fields, sheets and dialogs keep it.
        // Shortly after another key, Space is part of type-to-select.
        var lastKey = Date.distantPast
        keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] event in
            let typing = Date().timeIntervalSince(lastKey) < 1
            if event.keyCode != 49, event.modifierFlags.intersection([.command, .control]).isEmpty,
               event.characters?.contains(where: { $0.isLetter || $0.isNumber }) == true {
                lastKey = Date() // a letter or digit: type-to-select
            }
            guard event.keyCode == 49, // space
                  !event.isARepeat, !typing,
                  event.modifierFlags.intersection([.command, .option, .control, .shift]).isEmpty,
                  let window = event.window, !window.isSheet, window.attachedSheet == nil,
                  !(window.firstResponder is NSText), !(window.firstResponder is NSButton),
                  let self else { return event }
            MainActor.assumeIsolated { self.player.toggle() }
            return nil
        }
    }

    /// Polls the core and reloads the library when the index changed.
    func refresh() {
        let new = Core.status()
        // A sync that ended since the last load, even one that started and
        // ended between two polls.
        let finished = !new.isBusy && new.job.finished != loadedFinish
        if new != status { statusModel.status = new }
        let changed = new.dbEpoch != loadedEpoch || new.tracks != loadedCount
        // While indexing, reload at most every 2 seconds; at the end, always.
        if !new.dbPath.isEmpty, changed || finished, finished || Date().timeIntervalSince(lastLoad) > 2 {
            reload()
        }
    }

    private func reload() {
        do {
            if db == nil || status.dbEpoch != loadedEpoch {
                db = try LibraryDB(path: status.dbPath)
                art.db = db
                art.artDir = status.artDir
            }
            let read = try db?.tracksWithVersion()
            let newTracks = read?.tracks ?? []
            let version = read?.version ?? IndexVersion()
            // Rebuild what derives from the tracks only when they changed:
            // a sync that publishes changes the version, not the tracks.
            if newTracks != tracks {
                cloudIndex = CloudIndex(tracks: newTracks, version: version)
                tracks = newTracks
            } else if cloudIndex.version != version {
                cloudIndex.version = version
            }
            try reloadPlaylists()
            loadedEpoch = status.dbEpoch
            loadedCount = status.tracks
            loadedFinish = status.job.finished
            libraryError = nil
        } catch {
            db = nil // reopen on the next try
            libraryError = error.localizedDescription
        }
        lastLoad = Date()
    }

    /// Rereads the playlists, after the core changed them or the index
    /// changed. A playlist shown that no longer exists goes back to Songs.
    func reloadPlaylists() throws {
        let read = try db?.playlists() ?? []
        if read != playlists { playlists = read }
        if let id = selectedPlaylistID, !playlists.contains(where: { $0.id == id }) {
            sidebarSelection = .songs
        }
    }

    // MARK: Setup

    func connect() async throws {
        try await Core.connectGoogleDrive()
        refresh()
    }

    func useFolder(_ folder: String) throws {
        // Only the read connection closes first, so the core never deletes
        // the index file under it. What the window shows stays until the
        // core has agreed to the change.
        db = nil
        art.db = nil
        do {
            try Core.setFolder(folder)
        } catch {
            // Refused (unsaved playlist changes, say): reopen the index.
            reload()
            throw error
        }
        player.stop()
        tracks = []
        cloudIndex = CloudIndex()
        playlists = []
        sidebarSelection = .songs
        loadedEpoch = -1
        refresh()
    }

    func resync() { try? Core.startSync() }

    // MARK: Sources

    @Published var addingSource = false

    func addSecondary(remote: String, folder: String) throws {
        try Core.addSecondary(remote: remote, folder: folder)
        refresh()
    }

    func removeSecondary(_ id: String) throws {
        try Core.removeSecondary(id)
        refresh()
    }

    /// Makes a secondary the primary. The core does it as a background
    /// job: a sync, the switch, and a sync of the old primary.
    func promote(_ id: String) throws {
        try Core.promote(id)
        refresh()
    }

    func approveChanges(_ id: String) throws {
        try Core.approveChanges(id)
        refresh()
    }

    // MARK: Search

    func focusSearch() { searchFocusRequest += 1 }

    private func applySearch() {
        searched = search.filter(tracks, query: searchText)
        // Drop a browser choice the search has hidden, or the table would
        // be empty with nothing selected in that column.
        if let g = genre, !genres.contains(g) { genre = nil }
        if let c = composer, !composers.contains(c) { composer = nil }
        if let a = artist, !artists.contains(a) { artist = nil }
        if let a = album, !albums.contains(a) { album = nil }
    }

    // MARK: Column browser

    private func values(_ tracks: [Track], _ key: (Track) -> String) -> [String] {
        Array(Set(tracks.map(key).filter { !$0.isEmpty })).sorted {
            $0.localizedStandardCompare($1) == .orderedAscending
        }
    }

    var genres: [String] { values(searched, \.genre) }
    private var byGenre: [Track] { genre.map { g in searched.filter { $0.genre == g } } ?? searched }
    var composers: [String] { values(byGenre, \.composer) }
    private var byComposer: [Track] { composer.map { c in byGenre.filter { $0.composer == c } } ?? byGenre }
    var artists: [String] { values(byComposer, \.browseArtist) }
    private var byArtist: [Track] { artist.map { a in byComposer.filter { $0.browseArtist == a } } ?? byComposer }
    var albums: [String] { values(byArtist, \.album) }

    /// What visibleTracks depends on. The model publishes on every status
    /// poll, so the views ask for it far more often than it changes.
    private struct VisibleKey: Equatable {
        var searched: Int
        var playlists: Int
        var sidebar: SidebarItem
        var genre, composer, artist, album: String?
        var librarySort: [KeyPathComparator<Track>]
        var playlistSort: [KeyPathComparator<Track>]
    }

    /// The tracks the browser or the playlist shows, in the table's order,
    /// kept until something they depend on changes.
    var visibleTracks: [Track] {
        let key = VisibleKey(searched: searchedGeneration, playlists: playlistsGeneration, sidebar: sidebarSelection,
                             genre: genre, composer: composer, artist: artist, album: album,
                             librarySort: librarySortOrder, playlistSort: playlistSortOrder)
        if let cache = visibleCache, cache.key == key { return cache.tracks }
        let result = computeVisibleTracks()
        visibleCache = (key, result)
        return result
    }

    private func computeVisibleTracks() -> [Track] {
        if let id = selectedPlaylistID {
            // The search filters a playlist too; matches keep playlist order.
            var list = playlistTracks(id)
            if let keep = searchedIDs { list = list.filter { keep.contains($0.id) } }
            return playlistSortOrder.isEmpty ? list : list.sorted(using: playlistSortOrder)
        }
        let selected = album.map { a in byArtist.filter { $0.album == a } } ?? byArtist
        return selected.sorted(using: librarySortOrder)
    }

    func play(_ track: Track) {
        let queue = visibleTracks
        guard let base = URL(string: status.streamBase) else { return }
        player.play(queue: queue, startAt: queue.firstIndex(of: track) ?? 0, base: base)
    }
}

/// The playback position, published separately from Player so that
/// views that only need the current track don't redraw on every tick.
@MainActor
final class PlaybackClock: ObservableObject {
    @Published var position: Double = 0 // seconds
    /// Seconds from play() to the first audio, for the 2-second target.
    @Published var startLatency: Double?
}

/// Plays a queue of tracks from the core's localhost stream with AVPlayer.
@MainActor
final class Player: ObservableObject {
    @Published private(set) var current: Track?
    /// Playing or about to: false only when paused.
    @Published private(set) var isPlaying = false
    /// Audio is actually coming out: not paused, and not waiting for data.
    @Published private(set) var isAudible = false
    @Published private(set) var error: String?
    let clock = PlaybackClock()
    var position: Double { clock.position }

    private let avPlayer = AVPlayer()
    private var queue: [Track] = []
    private var index = 0
    private var base: URL?
    private var started = Date()
    private var observers: [Any] = []
    private var statusObservation: NSKeyValueObservation?

    init() {
        avPlayer.automaticallyWaitsToMinimizeStalling = true
        observers.append(avPlayer.addPeriodicTimeObserver(
            forInterval: CMTime(seconds: 0.25, preferredTimescale: 600), queue: .main
        ) { [weak self] time in
            MainActor.assumeIsolated {
                guard let self, self.current != nil else { return }
                self.clock.position = time.seconds
                if self.clock.startLatency == nil, self.isPlaying, time.seconds > 0 {
                    self.clock.startLatency = Date().timeIntervalSince(self.started)
                }
            }
        })
        observers.append(NotificationCenter.default.addObserver(
            forName: AVPlayerItem.didPlayToEndTimeNotification, object: nil, queue: .main
        ) { [weak self] note in
            MainActor.assumeIsolated {
                // Only the item playing now: a late notice from the item the
                // user just replaced must not skip the new one.
                guard let self, let item = note.object as? AVPlayerItem,
                      item === self.avPlayer.currentItem else { return }
                self.next()
            }
        })
        statusObservation = avPlayer.observe(\.timeControlStatus) { [weak self] p, _ in
            DispatchQueue.main.async {
                MainActor.assumeIsolated {
                    self?.isPlaying = p.timeControlStatus != .paused
                    self?.isAudible = p.timeControlStatus == .playing
                }
            }
        }
    }

    var duration: Double { Double(current?.durationMS ?? 0) / 1000 }

    func play(queue: [Track], startAt: Int, base: URL) {
        self.queue = queue
        self.base = base
        playIndex(startAt)
    }

    private func playIndex(_ i: Int) {
        guard queue.indices.contains(i), let base else { return stop() }
        index = i
        let track = queue[i]
        current = track
        error = nil
        clock.startLatency = nil
        clock.position = 0
        started = Date()
        if queue.indices.contains(i + 1) { Core.prepare(queue[i + 1].id) }
        let url = base.appendingPathComponent("\(track.id).\(track.format)")
        let item = AVPlayerItem(url: url)
        itemObservation = item.observe(\.status) { [weak self] item, _ in
            guard item.status == .failed else { return }
            let message = item.error?.localizedDescription ?? "playback failed"
            DispatchQueue.main.async {
                MainActor.assumeIsolated {
                    guard let self, item === self.avPlayer.currentItem else { return }
                    self.error = message
                    self.isPlaying = false
                    self.isAudible = false
                }
            }
        }
        avPlayer.replaceCurrentItem(with: item)
        avPlayer.play()
    }

    private var itemObservation: NSKeyValueObservation?

    func toggle() {
        if current == nil { return }
        if avPlayer.timeControlStatus == .paused { avPlayer.play() } else { avPlayer.pause() }
    }

    func next() { playIndex(index + 1) }

    func previous() {
        if position > 3 { seek(to: 0) } else { playIndex(max(index - 1, 0)) }
    }

    /// Seeks; AVPlayer then asks the core's stream for a new byte range.
    func seek(to seconds: Double) {
        clock.position = seconds // show it now, not when the seek lands
        avPlayer.seek(to: CMTime(seconds: seconds, preferredTimescale: 600))
    }

    func stop() {
        avPlayer.replaceCurrentItem(with: nil)
        current = nil
        clock.position = 0
    }
}
