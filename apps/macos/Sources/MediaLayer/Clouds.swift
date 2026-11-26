import SwiftUI

// The Clouds column: which of the library's sources hold a track.
//
// Nothing per source is stored per track. Every change to the index
// becomes a new published version, so writing sync results into it would
// make a new version after every sync. Instead the column follows from
// what is already known:
//
// - The primary holds every track the index lists: the index is made
//   from the primary's files.
// - A secondary whose last sync this session ended in `synced` holds
//   every track of the index version it copied. The core reports that
//   version, and the app reads the index's own version (and whether it
//   has changed since) in the same transaction as the tracks. When they
//   match, the secondary holds every listed track, except the paths that
//   sync leaves alone (docs/sync.md, Known gaps): paths that collide in
//   Unicode NFC, and on local folders also ignoring case. CloudIndex finds
//   those among the indexed paths.
// - Any other secondary (copying, paused, failed, synced to an older
//   version, or not yet synced since the app opened) may lack some
//   tracks, and which ones isn't known without listing it. Its icon is
//   dimmed on every row.

/// What the Clouds column needs from one read of the index.
struct CloudIndex: Equatable {
    var version = IndexVersion()
    /// Tracks whose paths collide with another's in Unicode NFC.
    var clashes: Set<Track.ID> = []
    /// Tracks whose paths collide in NFC ignoring case, as on a local disk.
    var foldedClashes: Set<Track.ID> = []

    init() {}

    init(tracks: [Track], version: IndexVersion) {
        self.version = version
        // Swift compares Strings by canonical equivalence, so two paths
        // with one NFC form are already one key.
        clashes = Self.colliding(tracks) { $0 }
        foldedClashes = Self.colliding(tracks) { $0.precomposedStringWithCanonicalMapping.lowercased() }
    }

    /// The tracks whose paths share a key with another track's path.
    private static func colliding(_ tracks: [Track], key: (String) -> String) -> Set<Track.ID> {
        var first: [String: Track.ID] = [:]
        var out: Set<Track.ID> = []
        for t in tracks {
            let k = key(t.path)
            if let other = first[k] {
                out.insert(other)
                out.insert(t.id)
            } else {
                first[k] = t.id
            }
        }
        return out
    }
}

/// One source's icon in a track's Clouds cell.
struct TrackCloud: Identifiable {
    let source: CoreStatus.Source
    /// The source certainly holds the track; otherwise it may not yet.
    let holds: Bool
    let help: String
    var id: String { source.id }
}

extension AppModel {
    /// The sources for a track's Clouds cell, primary first.
    func clouds(for track: Track) -> [TrackCloud] {
        (status.sources ?? []).map { (s) -> TrackCloud in
            let place = "“\(s.name)” (\(SourceIcon.label(s.type)))"
            if s.primary { return TrackCloud(source: s, holds: true, help: "On the primary, \(place)") }
            let current = s.state == "synced" && cloudIndex.version.version > 0
                && s.version == cloudIndex.version.version && !cloudIndex.version.dirty
            if current {
                let clash = (s.remote.isEmpty ? cloudIndex.foldedClashes : cloudIndex.clashes).contains(track.id)
                if !clash { return TrackCloud(source: s, holds: true, help: "On \(place), up to date") }
                return TrackCloud(source: s, holds: false,
                                  help: "Not kept on \(place): its path can't be told apart there from another track's")
            }
            let why = switch s.state {
            case "syncing": "copying to \(place) now"
            case "paused": "the sync to \(place) is paused"
            case "error": "the last sync to \(place) failed"
            case "synced": "\(place) has an earlier version of the library; the next sync updates it"
            default: "\(place) hasn't been synced since MediaLayer opened"
            }
            return TrackCloud(source: s, holds: false, help: "May not be on \(place) yet: \(why)")
        }
    }
}

/// The icons in a Clouds cell: one per source, dimmed where the source
/// may not hold the track.
struct CloudsCell: View {
    let clouds: [TrackCloud]

    var body: some View {
        HStack(spacing: 4) {
            ForEach(clouds) { c in
                SourceIcon(type: c.source.type, size: 11)
                    .opacity(c.holds ? 1 : 0.3)
                    .help(c.help)
            }
        }
        .foregroundStyle(.secondary)
    }
}
