import AVFoundation
import Foundation
import SwiftUI

/// The app's state: the core's status, the library, the column browser's
/// selection and the player.
@MainActor
final class AppModel: ObservableObject {
    @Published private(set) var status = CoreStatus()
    @Published private(set) var startError: String?
    /// The last failure reading the index; cleared by the next good read.
    @Published private(set) var libraryError: String?
    @Published private(set) var tracks: [Track] = []
    @Published var choosingFolder = false

    // Column browser: nil means "All".
    @Published var genre: String? { didSet { if genre != oldValue { composer = nil; artist = nil; album = nil } } }
    @Published var composer: String? { didSet { if composer != oldValue { artist = nil; album = nil } } }
    @Published var artist: String? { didSet { if artist != oldValue { album = nil } } }
    @Published var album: String?
    @Published var sortOrder = [KeyPathComparator(\Track.browseArtist), KeyPathComparator(\Track.album),
                                KeyPathComparator(\Track.discNumber), KeyPathComparator(\Track.trackNumber)]

    let art = ArtCache()
    let player = Player()

    private var db: LibraryDB?
    private var loadedEpoch: Int64 = -1
    private var loadedCount: Int64 = -1
    private var lastLoad = Date.distantPast
    private var loadedFinish: String?
    private var timer: Timer?

    init() {
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
    }

    /// Polls the core and reloads the library when the index changed.
    func refresh() {
        let new = Core.status()
        // A sync that ended since the last load, even one that started and
        // ended between two polls.
        let finished = !new.isBusy && new.job.finished != loadedFinish
        if new != status { status = new }
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
            tracks = try db?.tracks() ?? []
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

    // MARK: Setup

    func connect() async throws {
        try await Core.connectGoogleDrive()
        refresh()
    }

    func useFolder(_ folder: String) throws {
        player.stop()
        db = nil
        art.db = nil
        tracks = []
        loadedEpoch = -1
        try Core.setFolder(folder)
        refresh()
    }

    func resync() { try? Core.startSync() }

    // MARK: Column browser

    private func values(_ tracks: [Track], _ key: (Track) -> String) -> [String] {
        Array(Set(tracks.map(key).filter { !$0.isEmpty })).sorted {
            $0.localizedStandardCompare($1) == .orderedAscending
        }
    }

    var genres: [String] { values(tracks, \.genre) }
    private var byGenre: [Track] { genre.map { g in tracks.filter { $0.genre == g } } ?? tracks }
    var composers: [String] { values(byGenre, \.composer) }
    private var byComposer: [Track] { composer.map { c in byGenre.filter { $0.composer == c } } ?? byGenre }
    var artists: [String] { values(byComposer, \.browseArtist) }
    private var byArtist: [Track] { artist.map { a in byComposer.filter { $0.browseArtist == a } } ?? byComposer }
    var albums: [String] { values(byArtist, \.album) }

    /// The tracks the browser selects, in the table's order.
    var visibleTracks: [Track] {
        let selected = album.map { a in byArtist.filter { $0.album == a } } ?? byArtist
        return selected.sorted(using: sortOrder)
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
    @Published private(set) var isPlaying = false
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
                MainActor.assumeIsolated { self?.isPlaying = p.timeControlStatus != .paused }
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

    func seek(to seconds: Double) {
        avPlayer.seek(to: CMTime(seconds: seconds, preferredTimescale: 600))
    }

    func stop() {
        avPlayer.replaceCurrentItem(with: nil)
        current = nil
        clock.position = 0
    }
}
