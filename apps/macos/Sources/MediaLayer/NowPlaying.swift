import AppKit
import Combine
import Foundation
import MediaPlayer

/// Connects the player to the system's Now Playing: Control Centre and
/// the menu bar show the track, and the keyboard's media keys (F7 to F9),
/// headphone buttons and Control Centre's buttons and scrubber reach the
/// player through MPRemoteCommandCenter.
///
/// It watches the player and changes nothing in it:
/// 1. A new track publishes its title, artist, album, duration and cover,
///    and turns the commands on.
/// 2. Play and pause publish the elapsed time and the rate (1 or 0) and
///    set the playback state. The system counts the time on from there.
/// 3. A position more than a second away from that count (a seek, or a
///    stall) publishes the elapsed time again.
/// 4. Stop and quit clear the information and turn the commands off.
@MainActor
final class NowPlaying {
    private let player: Player
    private let art: ArtCache
    private var subscriptions: Set<AnyCancellable> = []
    private var targets: [(command: MPRemoteCommand, target: Any)] = []
    private var info: [String: Any] = [:]
    private var trackID: String?
    private var isPlaying = false
    /// The elapsed time last published, and when: the system counts on
    /// from it at the published rate.
    private var lastElapsed = 0.0
    private var lastUpdate = Date()

    private static let artSize = 600 // pixels on the long side

    init(player: Player, art: ArtCache) {
        self.player = player
        self.art = art
        let center = MPRemoteCommandCenter.shared()
        // The player has none of these, so the system shows no button for them.
        for command in [center.stopCommand, center.skipForwardCommand, center.skipBackwardCommand,
                        center.seekForwardCommand, center.seekBackwardCommand,
                        center.changeRepeatModeCommand, center.changeShuffleModeCommand,
                        center.changePlaybackRateCommand, center.ratingCommand,
                        center.likeCommand, center.dislikeCommand, center.bookmarkCommand] {
            command.isEnabled = false
        }
        // @Published sends the new value before it is stored, so each
        // handler uses the value it gets, not the player's property.
        player.$current
            .sink { [weak self] track in self?.trackChanged(track) }
            .store(in: &subscriptions)
        player.$isPlaying
            .removeDuplicates()
            .sink { [weak self] playing in self?.playingChanged(playing) }
            .store(in: &subscriptions)
        player.clock.$position
            .sink { [weak self] position in self?.positionChanged(position) }
            .store(in: &subscriptions)
        NotificationCenter.default.publisher(for: NSApplication.willTerminateNotification)
            .sink { [weak self] _ in self?.clear() }
            .store(in: &subscriptions)
    }

    // MARK: Player events

    private func trackChanged(_ track: Track?) {
        guard let track else { return clear() }
        if targets.isEmpty { addTargets() }
        trackID = track.id
        var info: [String: Any] = [
            MPMediaItemPropertyTitle: track.displayTitle,
            MPNowPlayingInfoPropertyMediaType: MPNowPlayingInfoMediaType.audio.rawValue,
            MPNowPlayingInfoPropertyDefaultPlaybackRate: 1.0,
        ]
        if !track.artist.isEmpty { info[MPMediaItemPropertyArtist] = track.artist }
        if !track.albumArtist.isEmpty { info[MPMediaItemPropertyAlbumArtist] = track.albumArtist }
        if !track.album.isEmpty { info[MPMediaItemPropertyAlbumTitle] = track.album }
        if track.durationMS > 0 {
            info[MPMediaItemPropertyPlaybackDuration] = Double(track.durationMS) / 1000
        }
        self.info = info
        publish(elapsed: 0)
        loadArtwork(track)
    }

    private func playingChanged(_ playing: Bool) {
        isPlaying = playing
        guard trackID != nil else { return }
        publish(elapsed: player.position)
    }

    private func positionChanged(_ position: Double) {
        guard trackID != nil else { return }
        let counted = lastElapsed + (isPlaying ? Date().timeIntervalSince(lastUpdate) : 0)
        if abs(position - counted) > 1 { publish(elapsed: position) }
    }

    private func loadArtwork(_ track: Track) {
        guard let sha = track.artSHA else { return }
        // ArtCache calls back on the main thread, at once when it has the image.
        art.load(sha, size: Self.artSize) { [weak self] image in
            MainActor.assumeIsolated {
                guard let self, let image, self.trackID == track.id else { return }
                self.info[MPMediaItemPropertyArtwork] = Self.artwork(image)
                self.publish(elapsed: self.player.position)
            }
        }
    }

    // MARK: System Now Playing

    private func publish(elapsed: Double) {
        lastElapsed = elapsed
        lastUpdate = Date()
        info[MPNowPlayingInfoPropertyElapsedPlaybackTime] = elapsed
        info[MPNowPlayingInfoPropertyPlaybackRate] = isPlaying ? 1.0 : 0.0
        let center = MPNowPlayingInfoCenter.default()
        center.nowPlayingInfo = info
        // macOS doesn't infer the state from the rate.
        center.playbackState = isPlaying ? .playing : .paused
    }

    /// Clears Now Playing and turns the commands off. Runs on stop and
    /// when the app quits.
    private func clear() {
        guard trackID != nil || !targets.isEmpty else { return }
        for t in targets {
            t.command.removeTarget(t.target)
            t.command.isEnabled = false
        }
        targets = []
        trackID = nil
        info = [:]
        let center = MPNowPlayingInfoCenter.default()
        center.nowPlayingInfo = nil
        center.playbackState = .stopped
    }

    private func addTargets() {
        let center = MPRemoteCommandCenter.shared()
        func add(_ command: MPRemoteCommand,
                 _ handler: @escaping (MPRemoteCommandEvent) -> MPRemoteCommandHandlerStatus) {
            command.isEnabled = true
            targets.append((command, command.addTarget(handler: handler)))
        }
        add(center.playCommand, Self.handler(player) { if !$0.isPlaying { $0.toggle() } })
        add(center.pauseCommand, Self.handler(player) { if $0.isPlaying { $0.toggle() } })
        add(center.togglePlayPauseCommand, Self.handler(player) { $0.toggle() })
        add(center.nextTrackCommand, Self.handler(player) { $0.next() })
        add(center.previousTrackCommand, Self.handler(player) { $0.previous() })
        add(center.changePlaybackPositionCommand, Self.seekHandler(player))
    }

    // The closures below are made outside the main actor on purpose. The
    // system may call them on any thread, and a closure made in a
    // main-actor method would count as main-actor code.

    nonisolated private static func handler(
        _ player: Player, _ action: @escaping @MainActor @Sendable (Player) -> Void
    ) -> (MPRemoteCommandEvent) -> MPRemoteCommandHandlerStatus {
        { [weak player] _ in
            guard let player else { return .commandFailed }
            onMain { action(player) }
            return .success
        }
    }

    nonisolated private static func seekHandler(
        _ player: Player
    ) -> (MPRemoteCommandEvent) -> MPRemoteCommandHandlerStatus {
        { [weak player] event in
            guard let player, let event = event as? MPChangePlaybackPositionCommandEvent else {
                return .commandFailed
            }
            let seconds = event.positionTime
            onMain { player.seek(to: seconds) }
            return .success
        }
    }

    nonisolated private static func artwork(_ image: NSImage) -> MPMediaItemArtwork {
        MPMediaItemArtwork(boundsSize: image.size) { _ in image }
    }

    /// Runs work on the main actor: now when on the main thread, else soon.
    nonisolated private static func onMain(_ work: @escaping @MainActor @Sendable () -> Void) {
        if Thread.isMainThread {
            MainActor.assumeIsolated { work() }
        } else {
            DispatchQueue.main.async { MainActor.assumeIsolated { work() } }
        }
    }
}
