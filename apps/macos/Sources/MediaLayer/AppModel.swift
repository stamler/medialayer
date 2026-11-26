import AppKit
import AVFoundation
import Combine
import Foundation
import SwiftUI

/// The core's status, polled every half second. It is apart from AppModel
/// so that a poll redraws only the views that show it: the status bar, the
/// source chips and the menus. During a sync it changes on most polls.
@MainActor
final class StatusModel: ObservableObject {
    @Published var status = CoreStatus()
}

/// Tells the views that show the search or the column browser that they
/// changed. AppModel's search and browser properties are not @Published:
/// a keystroke in the search field must redraw the search field, the
/// column browser and the status bar, not the whole window.
@MainActor
final class BrowseSignal: ObservableObject {}

/// The app's state: the library, the column browser's selection, the
/// playlists and the player. The core's status is in `statusModel`; the
/// search and the column browser announce changes through `browse`.
@MainActor
final class AppModel: ObservableObject {
    let browse = BrowseSignal()
    let statusModel = StatusModel()
    /// The core's status as last polled. Views that show it observe
    /// `statusModel`; this model doesn't publish when it changes.
    var status: CoreStatus { statusModel.status }
    @Published private(set) var startError: String?
    /// The last failure reading the index; cleared by the next good read.
    @Published private(set) var libraryError: String?
    @Published private(set) var tracks: [Track] = [] {
        didSet {
            tracksGeneration += 1
            browseRank = nil
            trackByID = Dictionary(tracks.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
            search.invalidate()
            searched = search.filter(tracks, query: searchText)
        }
    }
    /// Raised each time `tracks` is set: the sorted library's cache key.
    private var tracksGeneration = 0
    /// The library's present tracks by file id, for playlists.
    private(set) var trackByID: [String: Track] = [:]
    /// The toolbar's search text; the column browser and table show only
    /// the tracks it leaves.
    var searchText = "" {
        willSet { browse.objectWillChange.send() }
        didSet { if searchText != oldValue { applySearch() } }
    }
    /// Bumped by Edit > Find; the search field focuses itself on a change.
    @Published private(set) var searchFocusRequest = 0
    /// The tracks the search leaves: all of them when it is empty.
    private(set) var searched: [Track] = [] {
        willSet { browse.objectWillChange.send() }
        didSet {
            searchedIDs = isSearching ? Set(searched.map(\.id)) : nil
            searchedGeneration += 1
        }
    }
    /// Raised each time `searched` is set, which every change to the tracks
    /// or the search does: part of visibleTracks's cache key.
    private var searchedGeneration = 0
    /// The ids of `searched` while a search is on; nil when the search is
    /// empty.
    private(set) var searchedIDs: Set<String>?
    /// Whether the search text has any word to search for.
    var isSearching: Bool { !TrackSearch.words(searchText).isEmpty }
    /// What the Clouds column needs from the index (see Clouds.swift).
    @Published private(set) var cloudIndex = CloudIndex()
    @Published var choosingFolder = false

    // Column browser: nil means "All".
    var genre: String? = nil {
        willSet { browse.objectWillChange.send() }
        didSet { if genre != oldValue { composer = nil; artist = nil; album = nil } }
    }
    var composer: String? = nil {
        willSet { browse.objectWillChange.send() }
        didSet { if composer != oldValue { artist = nil; album = nil } }
    }
    var artist: String? = nil {
        willSet { browse.objectWillChange.send() }
        didSet { if artist != oldValue { album = nil } }
    }
    var album: String? = nil {
        willSet { browse.objectWillChange.send() }
    }
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
    /// Raised each time visibleTracks is computed anew.
    private(set) var visibleVersion = 0
    private var summaryCache = TrackSummary()
    /// The library sorted by librarySortOrder, and what it was sorted from.
    private var sortedCache: (tracks: Int, order: [KeyPathComparator<Track>], sorted: [Track])?
    /// The column browser's four levels (see browseLevel).
    private var browseLevels: [BrowseLevel?] = [nil, nil, nil, nil]
    /// Every browser value of the library in display order, and each
    /// one's place in it (see values).
    private var browseRank: (rank: [String: Int], values: [String])?
    /// Debounces the sync that publishes playlist changes (Playlists.swift).
    var publishTask: Task<Void, Never>?
    /// The playlist whose name is being edited in the sidebar.
    @Published var renamingPlaylist: String?
    @Published var playlistError: String?

    let art = ArtCache()
    /// The Songs table's columns, kept in the user's defaults.
    let columns = SongColumns()
    /// What the Songs table shows (see SongsTable.swift and updateFeed).
    let tableFeed = SongsTableFeed()
    private var feedScheduled = false
    private var feedSubscriptions: Set<AnyCancellable> = []
    private let search = TrackSearch()
    let player = Player()
    /// Control Centre's Now Playing and the media keys.
    private var nowPlaying: NowPlaying?

    private var db: LibraryDB?
    private var loadedEpoch: Int64 = -1
    private var loadedCount: Int64 = -1
    private var loadedAudioHashed: Int64 = -1
    private var lastLoad = Date.distantPast
    private var loadedFinish: String?
    private var timer: Timer?
    private var keyMonitor: Any?

    init() {
        nowPlaying = NowPlaying(player: player, art: art)
        followForTable()
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

    // MARK: The Songs table's feed

    /// Any change of this model, the search, the status or the playing
    /// track schedules one updateFeed for the end of the run loop turn,
    /// however many changes there were.
    private func followForTable() {
        let changes: [ObservableObjectPublisher] = [objectWillChange, browse.objectWillChange,
                                                    statusModel.objectWillChange]
        for publisher in changes {
            publisher
                .sink { [weak self] _ in self?.scheduleFeed() }
                .store(in: &feedSubscriptions)
        }
        player.$current
            .sink { [weak self] _ in self?.scheduleFeed() }
            .store(in: &feedSubscriptions)
        scheduleFeed()
    }

    private func scheduleFeed() {
        guard !feedScheduled else { return }
        feedScheduled = true
        // After the change: objectWillChange is sent before it is stored.
        DispatchQueue.main.async { [weak self] in
            MainActor.assumeIsolated {
                guard let self else { return }
                self.feedScheduled = false
                self.updateFeed()
            }
        }
    }

    /// Gives the table what it shows. visibleTracks is cached, so this is
    /// cheap when nothing the table shows changed, and the feed publishes
    /// nothing then.
    private func updateFeed() {
        let visible = visibleTracks
        tableFeed.update(SongsTableState(
            tracks: visible, version: visibleVersion, sortOrder: sortOrder, playlistID: selectedPlaylistID,
            currentID: player.current?.id, clouds: CloudState(sources: status.sources ?? [], index: cloudIndex)))
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
        } else if !new.dbPath.isEmpty, let h = new.audioHashes, h.hashed != loadedAudioHashed,
                  SongColumns.isVisible(.audioHash, in: columns.visibility),
                  !h.running || Date().timeIntervalSince(lastLoad) > 10 {
            // The background hash pass fills in the Audio Hash column, while
            // it shows: every 10 seconds while the pass runs, and once when
            // it stops.
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
            let realTracks = read?.tracks ?? []
            let newTracks = Self.withFakeTracks(realTracks)
            let version = read?.version ?? IndexVersion()
            // Rebuild what derives from the tracks only when they changed:
            // a sync that publishes changes the version, not the tracks.
            if newTracks != tracks {
                cloudIndex = CloudIndex(tracks: realTracks, version: version)
                tracks = newTracks
            } else if cloudIndex.version != version {
                cloudIndex.version = version
            }
            try reloadPlaylists()
            loadedEpoch = status.dbEpoch
            loadedCount = status.tracks
            loadedAudioHashed = status.audioHashes?.hashed ?? -1
            loadedFinish = status.job.finished
            libraryError = nil
        } catch {
            db = nil // reopen on the next try
            libraryError = error.localizedDescription
        }
        lastLoad = Date()
    }

    /// MEDIALAYER_FAKE_TRACKS=n: show n tracks, made by repeating the
    /// library's (see Track.fakeCopy), to try the window with a large
    /// library. The copies exist only here; nothing writes them anywhere.
    private static let fakeTrackCount = Int(ProcessInfo.processInfo.environment["MEDIALAYER_FAKE_TRACKS"] ?? "") ?? 0

    private static func withFakeTracks(_ real: [Track]) -> [Track] {
        let n = fakeTrackCount
        guard n > real.count, !real.isEmpty else { return real }
        var out = real
        out.reserveCapacity(n)
        var copy = 1
        while out.count < n {
            for t in real where out.count < n { out.append(t.fakeCopy(copy)) }
            copy += 1
        }
        return out
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
    //
    // Each column lists the values of the tracks that the columns to its
    // left leave: Genres lists the search's tracks, Composers the tracks
    // of the chosen genre, and so on. Each level is kept until the search
    // or a choice to its left changes, so a view that asks again, or a
    // change in Albums, sorts nothing.

    /// One column's input tracks and the values it lists.
    private struct BrowseLevel {
        var key: BrowseKey
        var tracks: [Track]
        var values: [String]
    }

    private struct BrowseKey: Equatable {
        var searched: Int
        var picks: [String?] // the choices in the columns to the left
    }

    /// The field that browser column `level` lists: 0 Genres, 1 Composers,
    /// 2 Artists, 3 Albums.
    private static func browseValue(_ t: Track, _ level: Int) -> String {
        switch level {
        case 0: return t.genre
        case 1: return t.composer
        case 2: return t.browseArtist
        default: return t.album
        }
    }

    private func browseLevel(_ level: Int) -> BrowseLevel {
        let picks: [String?] = [genre, composer, artist]
        let key = BrowseKey(searched: searchedGeneration, picks: Array(picks.prefix(level)))
        if let cached = browseLevels[level], cached.key == key { return cached }
        var input = searched
        if level > 0 {
            input = browseLevel(level - 1).tracks
            if let pick = picks[level - 1] {
                input = input.filter { Self.browseValue($0, level - 1) == pick }
            }
        }
        let result = BrowseLevel(key: key, tracks: input, values: values(input, level))
        browseLevels[level] = result
        return result
    }

    /// The distinct, non-empty values of one field, in Finder order. The
    /// library's values are sorted once per change of the tracks; a list
    /// then sorts their places, which are integers.
    private func values(_ tracks: [Track], _ level: Int) -> [String] {
        let ranked = rankedBrowseValues()
        var seen = Set<Int>()
        for t in tracks {
            if let i = ranked.rank[Self.browseValue(t, level)] { seen.insert(i) }
        }
        return seen.sorted().map { ranked.values[$0] }
    }

    private func rankedBrowseValues() -> (rank: [String: Int], values: [String]) {
        if let r = browseRank { return r }
        var all = Set<String>()
        for t in tracks {
            for level in 0..<4 { all.insert(Self.browseValue(t, level)) }
        }
        all.remove("")
        let sorted = all.sorted { $0.localizedStandardCompare($1) == .orderedAscending }
        var rank = [String: Int](minimumCapacity: sorted.count)
        for (i, v) in sorted.enumerated() { rank[v] = i }
        browseRank = (rank, sorted)
        return (rank, sorted)
    }

    var genres: [String] { browseLevel(0).values }
    var composers: [String] { browseLevel(1).values }
    var artists: [String] { browseLevel(2).values }
    var albums: [String] { browseLevel(3).values }

    /// What visibleTracks depends on. The views ask for it far more often
    /// than it changes. Parts that don't apply to what the sidebar shows
    /// are left at their defaults, so changing them recomputes nothing.
    private struct VisibleKey: Equatable {
        var searched: Int
        var playlists = 0
        var sidebar: SidebarItem
        var genre, composer, artist, album: String?
        var sort: [KeyPathComparator<Track>]
    }

    /// The tracks the browser or the playlist shows, in the table's order,
    /// kept until something they depend on changes.
    var visibleTracks: [Track] {
        var key = VisibleKey(searched: searchedGeneration, sidebar: sidebarSelection, sort: sortOrder)
        if selectedPlaylistID != nil {
            key.playlists = playlistsGeneration
        } else {
            key.genre = genre
            key.composer = composer
            key.artist = artist
            key.album = album
        }
        if let cache = visibleCache, cache.key == key { return cache.tracks }
        let result = computeVisibleTracks()
        visibleCache = (key, result)
        visibleVersion += 1
        summaryCache = TrackSummary(result)
        return result
    }

    /// The count, length and size of visibleTracks, for the status bar.
    var visibleSummary: TrackSummary {
        _ = visibleTracks
        return summaryCache
    }

    private func computeVisibleTracks() -> [Track] {
        if let id = selectedPlaylistID {
            // The search filters a playlist too; matches keep playlist order.
            var list = playlistTracks(id)
            if let keep = searchedIDs { list = list.filter { keep.contains($0.id) } }
            return playlistSortOrder.isEmpty ? list : list.sorted(using: playlistSortOrder)
        }
        // Filtering keeps the order, so the library is sorted once per
        // change of the tracks or the sort, not once per keystroke.
        let sorted = sortedLibrary()
        let keep = searchedIDs
        let g = genre, c = composer, a = artist, al = album
        if keep == nil, g == nil, c == nil, a == nil, al == nil { return sorted }
        return sorted.filter { t in
            (keep?.contains(t.id) ?? true) && (g == nil || t.genre == g) && (c == nil || t.composer == c)
                && (a == nil || t.browseArtist == a) && (al == nil || t.album == al)
        }
    }

    private func sortedLibrary() -> [Track] {
        if let c = sortedCache, c.tracks == tracksGeneration, c.order == librarySortOrder { return c.sorted }
        let sorted = tracks.sorted(using: librarySortOrder)
        sortedCache = (tracksGeneration, librarySortOrder, sorted)
        return sorted
    }

    func play(_ track: Track) {
        let queue = visibleTracks
        guard let base = URL(string: status.streamBase) else { return }
        player.play(queue: queue, startAt: queue.firstIndex(of: track) ?? 0, base: base)
    }
}

/// A list of tracks' count, length and size, as the status bar shows it.
struct TrackSummary: Equatable {
    var count = 0
    var durationMS: Int64 = 0
    var bytes: Int64 = 0

    init() {}

    init(_ tracks: [Track]) {
        count = tracks.count
        for t in tracks {
            durationMS += t.durationMS
            bytes += t.size
        }
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
    /// The extension to ask the core's stream for. Apple's decoders play
    /// FLAC at 16 and 24 bits but not FLAC in Ogg or at other depths, so
    /// those come decoded, as WAVE.
    static func streamExtension(_ t: Track) -> String {
        switch t.format {
        case "oga": return "wav"
        case "flac" where t.bitDepth != 16 && t.bitDepth != 24: return "wav"
        default: return t.format
        }
    }

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
        if queue.indices.contains(i + 1) { Core.prepare(queue[i + 1].fileID) }
        let url = base.appendingPathComponent("\(track.fileID).\(Player.streamExtension(track))")
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
