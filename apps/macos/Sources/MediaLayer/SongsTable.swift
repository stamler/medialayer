import SwiftUI

/// A table of tracks, as in iTunes' Songs view. Each column can be shown
/// or hidden from the header's context menu or View > Columns, and
/// reordered by dragging its header; the choice is kept across launches.
/// Any list of tracks (the library, a playlist) can use it.
struct SongsTable: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var player: Player
    @EnvironmentObject var columns: SongColumns
    let tracks: [Track]
    @Binding var selection: Set<Track.ID>

    var body: some View {
        Table(tracks, selection: $selection, sortOrder: $model.sortOrder,
              columnCustomization: $columns.customization) {
            // A column builder takes at most ten columns, so they come in groups.
            Group {
                TableColumn(SongColumn.art.title) { t in
                    ZStack {
                        ArtView(sha: t.artSHA, size: 18)
                        if player.current?.id == t.id {
                            Image(systemName: "speaker.wave.2.fill").font(.caption2)
                                .foregroundStyle(.white).shadow(radius: 2)
                        }
                    }
                }
                .width(28)
                .customizationID(SongColumn.art.id)
                .defaultVisibility(SongColumn.art.defaultVisibility)
                TableColumn(SongColumn.name.title, value: \.displayTitle)
                    .width(min: 120, ideal: 240)
                    .customizationID(SongColumn.name.id)
                    .disabledCustomizationBehavior(.visibility) // a table with no Name says little
                TableColumn(SongColumn.time.title, value: \.durationMS) { t in
                    Text(formatDuration(t.durationMS)).monospacedDigit()
                }
                .width(50)
                .customizationID(SongColumn.time.id)
                .defaultVisibility(SongColumn.time.defaultVisibility)
                TableColumn(SongColumn.artist.title, value: \.artist)
                    .width(min: 80, ideal: 160)
                    .customizationID(SongColumn.artist.id)
                    .defaultVisibility(SongColumn.artist.defaultVisibility)
                TableColumn(SongColumn.albumArtist.title, value: \.albumArtist)
                    .width(min: 80, ideal: 160)
                    .customizationID(SongColumn.albumArtist.id)
                    .defaultVisibility(SongColumn.albumArtist.defaultVisibility)
                TableColumn(SongColumn.album.title, value: \.album)
                    .width(min: 80, ideal: 180)
                    .customizationID(SongColumn.album.id)
                    .defaultVisibility(SongColumn.album.defaultVisibility)
                TableColumn(SongColumn.genre.title, value: \.genre)
                    .width(min: 60, ideal: 90)
                    .customizationID(SongColumn.genre.id)
                    .defaultVisibility(SongColumn.genre.defaultVisibility)
                TableColumn(SongColumn.composer.title, value: \.composer)
                    .width(min: 80, ideal: 140)
                    .customizationID(SongColumn.composer.id)
                    .defaultVisibility(SongColumn.composer.defaultVisibility)
            }
            Group {
                TableColumn(SongColumn.year.title, value: \.year)
                    .width(44)
                    .customizationID(SongColumn.year.id)
                    .defaultVisibility(SongColumn.year.defaultVisibility)
                TableColumn(SongColumn.disc.title, value: \.discNumber) { t in
                    Text(t.discNumber > 0 ? "\(t.discNumber)" : "").monospacedDigit()
                }
                .width(36)
                .customizationID(SongColumn.disc.id)
                .defaultVisibility(SongColumn.disc.defaultVisibility)
                TableColumn(SongColumn.track.title, value: \.trackNumber) { t in
                    Text(t.trackNumber > 0 ? "\(t.trackNumber)" : "").monospacedDigit()
                }
                .width(30)
                .customizationID(SongColumn.track.id)
                .defaultVisibility(SongColumn.track.defaultVisibility)
                TableColumn(SongColumn.kind.title, value: \.format) { t in Text(Self.kind(t)) }
                    .width(min: 60, ideal: 90)
                    .customizationID(SongColumn.kind.id)
                    .defaultVisibility(SongColumn.kind.defaultVisibility)
                TableColumn(SongColumn.bitRate.title, value: \.bitRate) { t in
                    Text(t.bitRate > 0 ? "\(t.bitRate) kbps" : "").monospacedDigit()
                }
                .width(min: 60, ideal: 76)
                .customizationID(SongColumn.bitRate.id)
                .defaultVisibility(SongColumn.bitRate.defaultVisibility)
                TableColumn(SongColumn.size.title, value: \.size) { t in
                    Text(ByteCountFormatter.string(fromByteCount: t.size, countStyle: .file)).monospacedDigit()
                }
                .width(min: 56, ideal: 70)
                .customizationID(SongColumn.size.id)
                .defaultVisibility(SongColumn.size.defaultVisibility)
                TableColumn(SongColumn.modified.title, value: \.mtimeMS) { t in
                    Text(t.mtimeMS > 0 ? Self.dates.string(from: Date(timeIntervalSince1970: Double(t.mtimeMS) / 1000)) : "")
                        .monospacedDigit()
                }
                .width(min: 90, ideal: 130)
                .customizationID(SongColumn.modified.id)
                .defaultVisibility(SongColumn.modified.defaultVisibility)
                TableColumn(SongColumn.clouds.title) { t in
                    CloudsCell(clouds: model.clouds(for: t))
                }
                .width(min: 44, ideal: 60)
                .customizationID(SongColumn.clouds.id)
                .defaultVisibility(SongColumn.clouds.defaultVisibility)
            }
        }
        .contextMenu(forSelectionType: Track.ID.self) { ids in
            if let id = ids.first, let t = tracks.first(where: { $0.id == id }) {
                Button("Play") { model.play(t) }
            }
        } primaryAction: { ids in
            if let id = ids.first, let t = tracks.first(where: { $0.id == id }) { model.play(t) }
        }
    }

    private static let dates: DateFormatter = {
        let f = DateFormatter()
        f.dateStyle = .short
        f.timeStyle = .short
        return f
    }()

    private static func kind(_ t: Track) -> String {
        switch t.format {
        case "flac" where t.bitDepth > 0 && t.sampleRate > 0:
            return "FLAC \(t.bitDepth)/\(t.sampleRate / 1000)"
        case "mp3": return "MP3"
        default: return t.format.uppercased()
        }
    }
}

/// The Songs table's columns, in their default order.
enum SongColumn: String, CaseIterable, Identifiable {
    case art, name, time, artist, albumArtist, album, genre, composer, year, disc, track, kind,
         bitRate, size, modified, clouds

    var id: String { rawValue } // the column's customizationID; saved, so don't rename

    var title: String {
        switch self {
        case .art: "Art"
        case .name: "Name"
        case .time: "Time"
        case .artist: "Artist"
        case .albumArtist: "Album Artist"
        case .album: "Album"
        case .genre: "Genre"
        case .composer: "Composer"
        case .year: "Year"
        case .disc: "Disc"
        case .track: "#"
        case .kind: "Kind"
        case .bitRate: "Bit Rate"
        case .size: "Size"
        case .modified: "Date Modified"
        case .clouds: "Clouds"
        }
    }

    var visibleByDefault: Bool {
        switch self {
        case .albumArtist, .composer, .disc, .bitRate, .size, .modified: false
        default: true
        }
    }

    var defaultVisibility: Visibility { visibleByDefault ? .visible : .hidden }

    /// Name always shows.
    var canHide: Bool { self != .name }
}

/// Which Songs table columns show, and in what order, kept in the user's
/// defaults. The table's header menu and View > Columns both change it.
@MainActor
final class SongColumns: ObservableObject {
    @Published var customization: TableColumnCustomization<Track> {
        didSet { if customization != oldValue { save() } }
    }

    private let key: String

    init(key: String = "SongsTable.columns") {
        self.key = key
        if let data = UserDefaults.standard.data(forKey: key),
           let saved = try? JSONDecoder().decode(TableColumnCustomization<Track>.self, from: data) {
            customization = saved
        } else {
            customization = TableColumnCustomization()
        }
    }

    func isVisible(_ column: SongColumn) -> Bool {
        switch customization[visibility: column.id] {
        case .visible: true
        case .hidden: false
        default: column.visibleByDefault // not changed by the user
        }
    }

    func setVisible(_ column: SongColumn, _ visible: Bool) {
        guard column.canHide else { return }
        customization[visibility: column.id] = visible ? .visible : .hidden
    }

    func restoreDefaults() { customization = TableColumnCustomization() }

    private func save() {
        if let data = try? JSONEncoder().encode(customization) {
            UserDefaults.standard.set(data, forKey: key)
        }
    }
}

/// View > Columns: a checkbox per column.
struct ColumnsMenu: View {
    @ObservedObject var columns: SongColumns

    var body: some View {
        Menu("Columns") {
            ForEach(SongColumn.allCases) { c in
                Toggle(c.title, isOn: Binding(get: { columns.isVisible(c) }, set: { columns.setVisible(c, $0) }))
                    .disabled(!c.canHide)
            }
            Divider()
            Button("Restore Default Columns") { columns.restoreDefaults() }
        }
    }
}
