import AppKit
import Combine
import SwiftUI

// The Songs table, as in iTunes' Songs view: an AppKit NSTableView, because
// SwiftUI's Table re-evaluates its rows on every change and lags with
// thousands of tracks. Any list of tracks (the library, a playlist) uses it.
//
// The table doesn't get its data through SwiftUI updates. AppModel sets
// tableFeed when the shown tracks, the sort, the playing track or the
// sources change, and the table controller follows tableFeed:
//
// 1. Same rows in the same order: it redraws only the rows whose track
//    changed.
// 2. Any other change of the rows: it reloads the table. That costs only
//    the rows on screen, and the selection is kept by id.
// 3. A new sort: it moves the header's sort indicator.
// 4. A new playing track: it redraws the art cells of the old and the new
//    one.
// 5. New source states: it redraws the Clouds cells.
//
// Everything else the model does leaves the table alone. SwiftUI passes
// only the selection binding.

// MARK: - What the table shows

/// What the Songs table shows. AppModel sets it in tableFeed.
struct SongsTableState: Equatable {
    var tracks: [Track] = []
    /// Raised whenever `tracks` may have changed. == compares this, not
    /// the tracks.
    var version = 0
    var sortOrder: [KeyPathComparator<Track>] = []
    /// The playlist shown, or nil for Songs.
    var playlistID: String?
    /// The playing track, which gets a speaker in the Art column.
    var currentID: String?
    var clouds = CloudState()

    static func == (a: SongsTableState, b: SongsTableState) -> Bool {
        a.version == b.version && a.sortOrder == b.sortOrder && a.playlistID == b.playlistID
            && a.currentID == b.currentID && a.clouds == b.clouds
    }
}

/// The table's data, from AppModel.
@MainActor
final class SongsTableFeed: ObservableObject {
    @Published private(set) var state = SongsTableState()

    /// Publishes `new` if it differs from the current state.
    func update(_ new: SongsTableState) {
        if new != state { state = new }
    }
}

// MARK: - The SwiftUI view

/// The Songs table in SwiftUI. Only the selection goes through SwiftUI;
/// the rest comes from the model's tableFeed (see the top of this file).
struct SongsTable: NSViewRepresentable {
    let model: AppModel
    @Binding var selection: Set<Track.ID>

    func makeCoordinator() -> SongsTableController { SongsTableController(model: model) }

    func makeNSView(context: Context) -> NSScrollView {
        context.coordinator.selection = $selection
        return context.coordinator.scrollView
    }

    func updateNSView(_ view: NSScrollView, context: Context) {
        context.coordinator.selection = $selection
        context.coordinator.selectionChanged(selection)
    }
}

// MARK: - The table controller

/// Owns the NSTableView: its data source, delegate and menus.
@MainActor
final class SongsTableController: NSObject, NSTableViewDataSource, NSTableViewDelegate, NSMenuDelegate {
    static let rowHeight: CGFloat = 22

    let scrollView = NSScrollView()
    var selection: Binding<Set<Track.ID>>?

    private let table = SongsTableView()
    private let model: AppModel
    private let columns: SongColumns
    private var state = SongsTableState()
    /// Each shown track's row.
    private var rowByID: [String: Int] = [:]
    /// The selection binding's ids, as last seen or set here. It can hold
    /// tracks that aren't shown (hidden by the search, say).
    private var selectedIDs = Set<String>()
    // Set while this code changes the table, so the delegate callbacks
    // that the change causes don't write the change back.
    private var settingSelection = false
    private var settingSort = false
    private var settingColumns = false
    /// The scroll position of Songs ("") and of each playlist shown.
    private var scrollOrigins: [String: NSPoint] = [:]
    private let rowMenu = NSMenu()
    private let headerMenu = NSMenu()
    private var subscriptions: Set<AnyCancellable> = []

    init(model: AppModel) {
        self.model = model
        columns = model.columns
        super.init()
        setUpTable()
        // Each publisher sends its current value first.
        model.tableFeed.$state
            .sink { [weak self] state in self?.apply(state) }
            .store(in: &subscriptions)
        columns.$order
            .sink { [weak self] order in self?.applyOrder(order) }
            .store(in: &subscriptions)
        columns.$visibility
            .sink { [weak self] visibility in self?.applyVisibility(visibility) }
            .store(in: &subscriptions)
        columns.$restores
            .dropFirst()
            .sink { [weak self] _ in self?.resetWidths() }
            .store(in: &subscriptions)
    }

    private func setUpTable() {
        table.style = .inset
        table.usesAlternatingRowBackgroundColors = true
        table.usesAutomaticRowHeights = false
        table.rowHeight = Self.rowHeight
        table.allowsMultipleSelection = true
        table.allowsEmptySelection = true
        table.allowsColumnSelection = false
        table.allowsColumnReordering = true
        table.allowsColumnResizing = true
        table.allowsTypeSelect = true
        table.columnAutoresizingStyle = .uniformColumnAutoresizingStyle
        // Rows drag to a sidebar playlist (a copy) or within a playlist (a
        // move). validateDrop accepts drops only in a playlist.
        table.setDraggingSourceOperationMask([.copy, .move], forLocal: true)
        table.setDraggingSourceOperationMask(.copy, forLocal: false)
        table.registerForDraggedTypes([.mediaLayerTracks])
        table.draggingDestinationFeedbackStyle = .regular
        if table.headerView == nil { table.headerView = NSTableHeaderView() }
        for c in columns.order { table.addTableColumn(makeColumn(c)) }
        table.dataSource = self
        table.delegate = self
        table.target = self
        table.doubleAction = #selector(doubleClicked(_:))
        table.onReturn = { [weak self] in self?.playSelection() }
        table.onDelete = { [weak self] in self?.removeSelection() ?? false }
        rowMenu.delegate = self
        rowMenu.autoenablesItems = false
        table.menu = rowMenu
        headerMenu.delegate = self
        headerMenu.autoenablesItems = false
        table.headerView?.menu = headerMenu

        scrollView.documentView = table
        scrollView.hasVerticalScroller = true
        scrollView.hasHorizontalScroller = true
        scrollView.autohidesScrollers = true
        scrollView.borderType = .noBorder
    }

    private func makeColumn(_ c: SongColumn) -> NSTableColumn {
        let col = NSTableColumn(identifier: NSUserInterfaceItemIdentifier(c.id))
        col.title = c.title
        col.headerToolTip = c.title
        let w = c.width
        if w.fixed {
            col.minWidth = w.ideal
            col.maxWidth = w.ideal
            col.width = w.ideal
            col.resizingMask = []
        } else {
            col.minWidth = w.min
            col.maxWidth = 10_000
            col.width = max(columns.width(for: c), w.min)
            col.resizingMask = [.autoresizingMask, .userResizingMask]
        }
        col.isHidden = !columns.isVisible(c)
        if c.comparator(.forward) != nil {
            col.sortDescriptorPrototype = NSSortDescriptor(key: c.id, ascending: true)
        }
        return col
    }

    private func track(at row: Int) -> Track? {
        state.tracks.indices.contains(row) ? state.tracks[row] : nil
    }

    private func columnIndex(_ c: SongColumn) -> Int {
        table.column(withIdentifier: NSUserInterfaceItemIdentifier(c.id))
    }

    // MARK: Following the feed

    private func apply(_ new: SongsTableState) {
        let old = state
        state = new
        var reloaded = false
        if new.playlistID != old.playlistID {
            scrollOrigins[old.playlistID ?? ""] = scrollView.contentView.bounds.origin
            reloadAll()
            reloaded = true
            restoreScroll(scrollOrigins[new.playlistID ?? ""])
        } else if new.version != old.version {
            reloaded = updateRows(from: old.tracks)
        }
        if new.sortOrder != old.sortOrder { showSort(new.sortOrder) }
        if !reloaded {
            if new.currentID != old.currentID { reloadArt([old.currentID, new.currentID]) }
            if new.clouds != old.clouds { reloadColumn(.clouds) }
        }
    }

    /// Shows state.tracks after `old`. Returns whether it reloaded the
    /// whole table.
    private func updateRows(from old: [Track]) -> Bool {
        let new = state.tracks
        if new.count == old.count {
            var same = true
            var changed = IndexSet()
            for i in new.indices {
                if new[i].id != old[i].id {
                    same = false
                    break
                }
                if new[i] != old[i] { changed.insert(i) }
            }
            if same {
                if !changed.isEmpty {
                    table.reloadData(forRowIndexes: changed,
                                     columnIndexes: IndexSet(integersIn: 0..<table.numberOfColumns))
                }
                return false
            }
        }
        reloadAll()
        return true
    }

    private func reloadAll() {
        var byID = [String: Int](minimumCapacity: state.tracks.count)
        for (i, t) in state.tracks.enumerated() where byID[t.id] == nil { byID[t.id] = i }
        rowByID = byID
        // A reload keeps the selected row numbers, which now name other
        // tracks: select the selected ids' rows again.
        settingSelection = true
        table.reloadData()
        settingSelection = false
        selectRows(selectedIDs)
    }

    private func restoreScroll(_ origin: NSPoint?) {
        guard let origin else {
            if table.numberOfRows > 0 { table.scrollRowToVisible(0) }
            return
        }
        let clip = scrollView.contentView
        let bounds = clip.constrainBoundsRect(NSRect(origin: origin, size: clip.bounds.size))
        clip.scroll(to: bounds.origin)
        scrollView.reflectScrolledClipView(clip)
    }

    private func reloadArt(_ ids: [String?]) {
        let col = columnIndex(.art)
        guard col >= 0 else { return }
        var rows = IndexSet()
        for case let id? in ids {
            if let r = rowByID[id] { rows.insert(r) }
        }
        if !rows.isEmpty { table.reloadData(forRowIndexes: rows, columnIndexes: IndexSet(integer: col)) }
    }

    /// Redraws one column's cells. Only rows that have views do any work.
    private func reloadColumn(_ c: SongColumn) {
        let col = columnIndex(c)
        guard col >= 0, table.numberOfRows > 0 else { return }
        table.reloadData(forRowIndexes: IndexSet(integersIn: 0..<table.numberOfRows),
                         columnIndexes: IndexSet(integer: col))
    }

    // MARK: Sorting

    /// Shows the model's sort in the header. A comparator with no column
    /// (the library's default Album Artist sort) shows no indicator, and
    /// neither does an unsorted playlist.
    private func showSort(_ order: [KeyPathComparator<Track>]) {
        var descriptors: [NSSortDescriptor] = []
        for comp in order {
            guard let c = SongColumn.column(sortingBy: comp.keyPath) else { break }
            descriptors.append(NSSortDescriptor(key: c.id, ascending: comp.order == .forward))
        }
        settingSort = true
        table.sortDescriptors = descriptors
        settingSort = false
        for col in table.tableColumns { table.setIndicatorImage(nil, in: col) }
        if let first = descriptors.first, let key = first.key,
           let col = table.tableColumn(withIdentifier: NSUserInterfaceItemIdentifier(key)) {
            let name = first.ascending ? "NSAscendingSortIndicator" : "NSDescendingSortIndicator"
            table.setIndicatorImage(NSImage(named: name), in: col)
        }
    }

    /// A header click: its column's comparator goes first, and the
    /// previous ones break ties. The model's change comes back through
    /// the feed and re-sorts the rows.
    func tableView(_ tableView: NSTableView, sortDescriptorsDidChange oldDescriptors: [NSSortDescriptor]) {
        guard !settingSort, let first = tableView.sortDescriptors.first, let key = first.key,
              let c = SongColumn(rawValue: key),
              let comp = c.comparator(first.ascending ? .forward : .reverse) else { return }
        var order = model.sortOrder.filter { $0.keyPath != comp.keyPath }
        order.insert(comp, at: 0)
        model.sortOrder = Array(order.prefix(6))
    }

    // MARK: Data source and cells

    func numberOfRows(in tableView: NSTableView) -> Int { state.tracks.count }

    func tableView(_ tableView: NSTableView, viewFor tableColumn: NSTableColumn?, row: Int) -> NSView? {
        guard let tableColumn, let c = SongColumn(rawValue: tableColumn.identifier.rawValue),
              let t = track(at: row) else { return nil }
        switch c {
        case .art:
            let cell = (tableView.makeView(withIdentifier: ArtCell.reuseID, owner: nil) as? ArtCell)
                ?? ArtCell(frame: NSRect(x: 0, y: 0, width: 28, height: Self.rowHeight))
            cell.show(sha: t.artSHA, playing: t.id == state.currentID, art: model.art)
            return cell
        case .clouds:
            let cell = (tableView.makeView(withIdentifier: CloudsCellView.reuseID, owner: nil) as? CloudsCellView)
                ?? CloudsCellView(frame: NSRect(x: 0, y: 0, width: 60, height: Self.rowHeight))
            cell.show(state.clouds.clouds(for: t))
            return cell
        default:
            let id = c.isNumeric ? TextCell.numberID : TextCell.textID
            let cell = (tableView.makeView(withIdentifier: id, owner: nil) as? TextCell)
                ?? TextCell(identifier: id, numeric: c.isNumeric)
            cell.textField?.stringValue = c.text(t)
            return cell
        }
    }

    /// A row scrolled away: stop loading its cover.
    func tableView(_ tableView: NSTableView, didRemove rowView: NSTableRowView, forRow row: Int) {
        for i in 0..<rowView.numberOfColumns {
            (rowView.view(atColumn: i) as? ArtCell)?.cancelLoad()
        }
    }

    /// Type-to-select matches the sorted column's text, as in iTunes, or
    /// the name when that column isn't text.
    func tableView(_ tableView: NSTableView, typeSelectStringFor tableColumn: NSTableColumn?, row: Int) -> String? {
        guard let t = track(at: row) else { return nil }
        if let keyPath = state.sortOrder.first?.keyPath, let text = t[keyPath: keyPath] as? String { return text }
        return t.displayTitle
    }

    // MARK: Selection

    /// The binding changed in SwiftUI (cleared on a sidebar change, say).
    func selectionChanged(_ ids: Set<String>) {
        guard ids != selectedIDs else { return }
        selectedIDs = ids
        selectRows(ids)
    }

    private func selectRows(_ ids: Set<String>) {
        var rows = IndexSet()
        for id in ids {
            if let r = rowByID[id] { rows.insert(r) }
        }
        guard rows != table.selectedRowIndexes else { return }
        settingSelection = true
        table.selectRowIndexes(rows, byExtendingSelection: false)
        settingSelection = false
    }

    func tableViewSelectionDidChange(_ notification: Notification) {
        guard !settingSelection else { return }
        let ids = Set(table.selectedRowIndexes.compactMap { track(at: $0)?.id })
        guard ids != selectedIDs else { return }
        selectedIDs = ids
        selection?.wrappedValue = ids
    }

    private var selectedTrackIDs: [String] {
        table.selectedRowIndexes.compactMap { track(at: $0)?.id }
    }

    // MARK: Playing and removing

    @objc private func doubleClicked(_ sender: Any?) {
        if let t = track(at: table.clickedRow) { model.play(t) }
    }

    /// Return plays the first selected row.
    private func playSelection() {
        if let r = table.selectedRowIndexes.first, let t = track(at: r) { model.play(t) }
    }

    /// Delete removes the selected rows from the playlist shown. Returns
    /// false in Songs, where Delete does nothing.
    private func removeSelection() -> Bool {
        guard let p = state.playlistID else { return false }
        let ids = selectedTrackIDs
        guard !ids.isEmpty else { return false }
        model.remove(ids, from: p)
        return true
    }

    // MARK: Menus

    func menuNeedsUpdate(_ menu: NSMenu) {
        menu.removeAllItems()
        if menu === headerMenu { buildHeaderMenu(menu) } else { buildRowMenu(menu) }
    }

    /// Play, Add to Playlist and Remove from Playlist, for the clicked row,
    /// or the selection when the clicked row is part of it.
    private func buildRowMenu(_ menu: NSMenu) {
        let clicked = table.clickedRow
        var rows = IndexSet()
        if clicked >= 0 {
            rows = table.selectedRowIndexes.contains(clicked) ? table.selectedRowIndexes : IndexSet(integer: clicked)
        }
        let ids = rows.compactMap { track(at: $0)?.id } // table order
        if let t = track(at: clicked) {
            menu.addItem(menuItem("Play") { [weak self] in self?.model.play(t) })
        }
        let add = NSMenuItem(title: "Add to Playlist", action: nil, keyEquivalent: "")
        let playlists = NSMenu()
        playlists.autoenablesItems = false
        playlists.addItem(menuItem("New Playlist") { [weak self] in self?.model.newPlaylist(with: ids) })
        if !model.playlists.isEmpty { playlists.addItem(.separator()) }
        for p in model.playlists {
            let id = p.id
            playlists.addItem(menuItem(p.name) { [weak self] in self?.model.add(ids, to: id) })
        }
        add.submenu = playlists
        add.isEnabled = !ids.isEmpty
        menu.addItem(add)
        if let p = model.selectedPlaylist {
            let id = p.id
            menu.addItem(menuItem("Remove from Playlist", enabled: !ids.isEmpty) { [weak self] in
                self?.model.remove(ids, from: id)
            })
        }
    }

    /// The header's menu: a checkbox per column, as in View > Columns.
    private func buildHeaderMenu(_ menu: NSMenu) {
        for c in SongColumn.allCases {
            let item = menuItem(c.title, enabled: c.canHide) { [weak self] in
                guard let self else { return }
                self.columns.setVisible(c, !self.columns.isVisible(c))
            }
            item.state = columns.isVisible(c) ? .on : .off
            menu.addItem(item)
        }
        menu.addItem(.separator())
        menu.addItem(menuItem("Restore Default Columns") { [weak self] in self?.columns.restoreDefaults() })
    }

    private func menuItem(_ title: String, enabled: Bool = true, action: @escaping () -> Void) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: #selector(runMenuItem(_:)), keyEquivalent: "")
        item.target = self
        item.representedObject = MenuAction(action)
        item.isEnabled = enabled
        return item
    }

    @objc private func runMenuItem(_ sender: NSMenuItem) {
        (sender.representedObject as? MenuAction)?.run()
    }

    // MARK: Columns

    private func applyOrder(_ order: [SongColumn]) {
        guard !settingColumns else { return }
        settingColumns = true
        for (target, c) in order.enumerated() {
            let current = columnIndex(c)
            if current >= 0, current != target { table.moveColumn(current, toColumn: target) }
        }
        settingColumns = false
    }

    private func applyVisibility(_ visibility: [String: Bool]) {
        for c in SongColumn.allCases {
            guard let col = table.tableColumn(withIdentifier: NSUserInterfaceItemIdentifier(c.id)) else { continue }
            let hidden = !SongColumns.isVisible(c, in: visibility)
            if col.isHidden != hidden { col.isHidden = hidden }
        }
    }

    private func resetWidths() {
        for c in SongColumn.allCases where !c.width.fixed {
            table.tableColumn(withIdentifier: NSUserInterfaceItemIdentifier(c.id))?.width = c.width.ideal
        }
    }

    /// A header drag moved a column.
    func tableViewColumnDidMove(_ notification: Notification) {
        guard !settingColumns else { return }
        settingColumns = true
        columns.setOrder(table.tableColumns.compactMap { SongColumn(rawValue: $0.identifier.rawValue) })
        settingColumns = false
    }

    /// Keeps widths the user drags, not the ones the table sets when the
    /// window resizes.
    func tableViewColumnDidResize(_ notification: Notification) {
        guard let header = table.headerView, header.resizedColumn >= 0,
              let col = notification.userInfo?["NSTableColumn"] as? NSTableColumn,
              let c = SongColumn(rawValue: col.identifier.rawValue) else { return }
        columns.setWidth(col.width, for: c)
    }

    // MARK: Dragging

    /// Each dragged row writes its own id; TrackDrag.ids joins a drop's
    /// items in order. The type and the JSON are what TrackDrag's
    /// CodableRepresentation reads, so the sidebar's SwiftUI
    /// dropDestination takes these drags.
    func tableView(_ tableView: NSTableView, pasteboardWriterForRow row: Int) -> NSPasteboardWriting? {
        guard let t = track(at: row),
              let data = try? JSONEncoder().encode(TrackDrag(ids: [t.id], playlist: state.playlistID))
        else { return nil }
        let item = NSPasteboardItem()
        _ = item.setData(data, forType: .mediaLayerTracks)
        return item
    }

    /// Only a playlist takes drops, between rows.
    func tableView(_ tableView: NSTableView, validateDrop info: NSDraggingInfo, proposedRow row: Int,
                   proposedDropOperation dropOperation: NSTableView.DropOperation) -> NSDragOperation {
        guard state.playlistID != nil else { return [] }
        if dropOperation == .on { tableView.setDropRow(row, dropOperation: .above) }
        return (info.draggingSource as? NSTableView) === tableView ? .move : .copy
    }

    func tableView(_ tableView: NSTableView, acceptDrop info: NSDraggingInfo, row: Int,
                   dropOperation: NSTableView.DropOperation) -> Bool {
        let drops = (info.draggingPasteboard.pasteboardItems ?? []).compactMap { item -> TrackDrag? in
            guard let data = item.data(forType: .mediaLayerTracks) else { return nil }
            return try? JSONDecoder().decode(TrackDrag.self, from: data)
        }
        guard !drops.isEmpty else { return false }
        model.dropOnTable(drops, at: row, rows: state.tracks)
        return true
    }
}

/// A menu item's action.
private final class MenuAction {
    let run: () -> Void
    init(_ run: @escaping () -> Void) { self.run = run }
}

/// The table, with Return to play and Delete to remove.
final class SongsTableView: NSTableView {
    var onReturn: (() -> Void)?
    /// Returns whether it removed anything; if not, the key beeps as usual.
    var onDelete: (() -> Bool)?

    override func keyDown(with event: NSEvent) {
        if event.modifierFlags.intersection([.command, .option, .control, .shift]).isEmpty {
            switch event.keyCode {
            case 36, 76: // Return, Enter
                onReturn?()
                return
            case 51, 117: // Delete, Forward Delete
                if onDelete?() == true { return }
            default:
                break
            }
        }
        super.keyDown(with: event) // type-to-select, arrows, ...
    }
}

// MARK: - Cells

/// A text cell: one label, centred vertically. Digits line up in the
/// number columns.
final class TextCell: NSTableCellView {
    static let textID = NSUserInterfaceItemIdentifier("SongsTable.text")
    static let numberID = NSUserInterfaceItemIdentifier("SongsTable.number")

    init(identifier: NSUserInterfaceItemIdentifier, numeric: Bool) {
        super.init(frame: NSRect(x: 0, y: 0, width: 100, height: SongsTableController.rowHeight))
        self.identifier = identifier
        let field = NSTextField(labelWithString: "")
        let size = NSFont.systemFontSize
        field.font = numeric ? .monospacedDigitSystemFont(ofSize: size, weight: .regular) : .systemFont(ofSize: size)
        field.lineBreakMode = .byTruncatingTail
        field.cell?.truncatesLastVisibleLine = true
        let height = ceil(field.intrinsicContentSize.height)
        field.frame = NSRect(x: 2, y: floor((bounds.height - height) / 2), width: bounds.width - 4, height: height)
        field.autoresizingMask = [.width, .minYMargin, .maxYMargin]
        addSubview(field)
        textField = field
    }

    required init?(coder: NSCoder) { fatalError("init(coder:) is not used") }
}

/// The Art cell: the cover, loaded when the row comes into view, and a
/// speaker on the playing track.
final class ArtCell: NSTableCellView {
    static let reuseID = NSUserInterfaceItemIdentifier("SongsTable.art")
    static let pixels = 36 // 18 points at 2x

    private let cover = CoverView(frame: .zero)
    private let speaker = NSImageView(frame: .zero)
    private var sha: String?
    private var loading: Task<Void, Never>?

    override init(frame frameRect: NSRect) {
        super.init(frame: frameRect)
        identifier = Self.reuseID
        let side: CGFloat = 18
        let box = NSRect(x: floor((frameRect.width - side) / 2), y: floor((frameRect.height - side) / 2),
                         width: side, height: side)
        for v in [cover, speaker] as [NSView] {
            v.frame = box
            v.autoresizingMask = [.minXMargin, .maxXMargin, .minYMargin, .maxYMargin]
            addSubview(v)
        }
        speaker.image = NSImage(systemSymbolName: "speaker.wave.2.fill", accessibilityDescription: "Playing")?
            .withSymbolConfiguration(NSImage.SymbolConfiguration(pointSize: 9, weight: .regular))
        speaker.imageScaling = .scaleNone
        speaker.contentTintColor = .white
        let shadow = NSShadow()
        shadow.shadowBlurRadius = 2
        shadow.shadowOffset = .zero
        shadow.shadowColor = NSColor.black.withAlphaComponent(0.6)
        speaker.wantsLayer = true
        speaker.shadow = shadow
        speaker.isHidden = true
    }

    required init?(coder: NSCoder) { fatalError("init(coder:) is not used") }

    func show(sha: String?, playing: Bool, art: ArtCache) {
        speaker.isHidden = !playing
        if sha == self.sha, cover.image != nil || loading != nil { return } // showing or loading it
        cancelLoad()
        self.sha = sha
        guard let sha else {
            cover.image = nil
            return
        }
        if let hit = art.cached(sha, size: Self.pixels) {
            cover.image = hit
            return
        }
        cover.image = nil
        loading = Task { [weak self] in
            let image = await art.image(sha, size: Self.pixels)
            guard let self, !Task.isCancelled, self.sha == sha else { return } // reused since
            self.cover.image = image
            self.loading = nil
        }
    }

    /// Stops a load nobody waits for any more; ArtCache then drops it.
    func cancelLoad() {
        loading?.cancel()
        loading = nil
        sha = nil
    }
}

/// A cover with rounded corners, filling its square, or a music note on
/// grey while there is none.
final class CoverView: NSView {
    var image: NSImage? {
        didSet {
            guard image !== oldValue else { return }
            note.isHidden = image != nil
            needsDisplay = true
        }
    }

    private let note = NSImageView(frame: .zero)

    override init(frame frameRect: NSRect) {
        super.init(frame: frameRect)
        wantsLayer = true
        layerContentsRedrawPolicy = .onSetNeedsDisplay
        note.image = NSImage(systemSymbolName: "music.note", accessibilityDescription: nil)?
            .withSymbolConfiguration(NSImage.SymbolConfiguration(pointSize: 8, weight: .regular))
        note.imageScaling = .scaleNone
        note.contentTintColor = .tertiaryLabelColor
        note.frame = bounds
        note.autoresizingMask = [.width, .height]
        addSubview(note)
    }

    required init?(coder: NSCoder) { fatalError("init(coder:) is not used") }

    override var wantsUpdateLayer: Bool { true }

    // AppKit calls this in the view's appearance, so the grey follows
    // light and dark mode.
    override func updateLayer() {
        guard let layer else { return }
        layer.cornerRadius = 2
        layer.masksToBounds = true
        layer.contentsGravity = .resizeAspectFill
        layer.contents = image
        layer.backgroundColor = image == nil ? NSColor.quaternaryLabelColor.cgColor : nil
    }
}

/// The Clouds cell: one icon per source, dimmed where the source may not
/// hold the track, with the reason as its tooltip.
final class CloudsCellView: NSTableCellView {
    static let reuseID = NSUserInterfaceItemIdentifier("SongsTable.clouds")
    private var icons: [NSImageView] = []

    override init(frame frameRect: NSRect) {
        super.init(frame: frameRect)
        identifier = Self.reuseID
    }

    required init?(coder: NSCoder) { fatalError("init(coder:) is not used") }

    func show(_ clouds: [TrackCloud]) {
        while icons.count < clouds.count {
            let v = NSImageView(frame: .zero)
            v.imageScaling = .scaleProportionallyDown
            v.contentTintColor = .secondaryLabelColor
            v.autoresizingMask = [.maxXMargin, .minYMargin, .maxYMargin]
            addSubview(v)
            icons.append(v)
        }
        let side: CGFloat = 12
        for (i, v) in icons.enumerated() {
            guard i < clouds.count else {
                v.isHidden = true
                continue
            }
            let c = clouds[i]
            v.isHidden = false
            v.image = CloudGlyphs.image(for: c.source.type)
            v.alphaValue = c.holds ? 1 : 0.3
            v.toolTip = c.help
            v.frame = NSRect(x: 2 + CGFloat(i) * (side + 4), y: floor((bounds.height - side) / 2),
                             width: side, height: side)
        }
    }
}

/// AppKit versions of SourceIcon's images, made once per cloud type.
@MainActor
enum CloudGlyphs {
    private static var cache: [String: NSImage] = [:]

    static func image(for type: String) -> NSImage? {
        if let hit = cache[type] { return hit }
        let image: NSImage? = type == "drive" ? driveGlyph()
            : NSImage(systemSymbolName: SourceIcon.symbol(type), accessibilityDescription: SourceIcon.label(type))?
                .withSymbolConfiguration(NSImage.SymbolConfiguration(pointSize: 10.5, weight: .regular))
        if let image { cache[type] = image }
        return image
    }

    /// DriveGlyph's triangle of three bands, as a template image.
    private static func driveGlyph() -> NSImage {
        let image = NSImage(size: NSSize(width: 11, height: 11 * 0.87), flipped: true) { rect in
            let r = rect.insetBy(dx: 0.65, dy: 0.65) // the stroke's half width
            let p = NSBezierPath()
            p.move(to: NSPoint(x: r.midX, y: r.minY))
            p.line(to: NSPoint(x: r.maxX, y: r.maxY))
            p.line(to: NSPoint(x: r.minX, y: r.maxY))
            p.close()
            p.move(to: NSPoint(x: r.minX + r.width * 0.25, y: r.midY))
            p.line(to: NSPoint(x: r.maxX - r.width * 0.25, y: r.midY))
            p.line(to: NSPoint(x: r.midX, y: r.maxY))
            p.close()
            p.lineWidth = 1.3
            p.lineJoinStyle = .round
            NSColor.black.setStroke()
            p.stroke()
            return true
        }
        image.isTemplate = true
        return image
    }
}

// MARK: - Columns

/// The Songs table's columns, in their default order.
enum SongColumn: String, CaseIterable, Identifiable {
    case art, name, time, artist, albumArtist, album, genre, composer, year, disc, track, kind,
         bitRate, size, modified, clouds

    var id: String { rawValue } // saved, so don't rename

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

    /// Name always shows.
    var canHide: Bool { self != .name }

    /// A fixed column has one width; the others start at `ideal` and
    /// shrink to `min`.
    var width: (min: CGFloat, ideal: CGFloat, fixed: Bool) {
        switch self {
        case .art: return (28, 28, true)
        case .name: return (120, 240, false)
        case .time: return (50, 50, true)
        case .artist, .albumArtist: return (80, 160, false)
        case .album: return (80, 180, false)
        case .genre: return (60, 90, false)
        case .composer: return (80, 140, false)
        case .year: return (44, 44, true)
        case .disc: return (36, 36, true)
        case .track: return (30, 30, true)
        case .kind: return (60, 90, false)
        case .bitRate: return (60, 76, false)
        case .size: return (56, 70, false)
        case .modified: return (90, 130, false)
        case .clouds: return (44, 60, false)
        }
    }

    /// Digits line up in these.
    var isNumeric: Bool {
        switch self {
        case .time, .disc, .track, .bitRate, .size, .modified: return true
        default: return false
        }
    }

    /// The column's sort, or nil for Art and Clouds, which don't sort.
    func comparator(_ order: SortOrder) -> KeyPathComparator<Track>? {
        switch self {
        case .art, .clouds: return nil
        case .name: return KeyPathComparator(\Track.displayTitle, order: order)
        case .time: return KeyPathComparator(\Track.durationMS, order: order)
        case .artist: return KeyPathComparator(\Track.artist, order: order)
        case .albumArtist: return KeyPathComparator(\Track.albumArtist, order: order)
        case .album: return KeyPathComparator(\Track.album, order: order)
        case .genre: return KeyPathComparator(\Track.genre, order: order)
        case .composer: return KeyPathComparator(\Track.composer, order: order)
        case .year: return KeyPathComparator(\Track.year, order: order)
        case .disc: return KeyPathComparator(\Track.discNumber, order: order)
        case .track: return KeyPathComparator(\Track.trackNumber, order: order)
        case .kind: return KeyPathComparator(\Track.format, order: order)
        case .bitRate: return KeyPathComparator(\Track.bitRate, order: order)
        case .size: return KeyPathComparator(\Track.size, order: order)
        case .modified: return KeyPathComparator(\Track.mtimeMS, order: order)
        }
    }

    /// The column whose header sorts by `keyPath`.
    static func column(sortingBy keyPath: PartialKeyPath<Track>) -> SongColumn? {
        allCases.first { $0.comparator(.forward)?.keyPath == keyPath }
    }

    /// A text column's text for a track.
    func text(_ t: Track) -> String {
        switch self {
        case .art, .clouds: return ""
        case .name: return t.displayTitle
        case .time: return formatDuration(t.durationMS)
        case .artist: return t.artist
        case .albumArtist: return t.albumArtist
        case .album: return t.album
        case .genre: return t.genre
        case .composer: return t.composer
        case .year: return t.year
        case .disc: return t.discNumber > 0 ? "\(t.discNumber)" : ""
        case .track: return t.trackNumber > 0 ? "\(t.trackNumber)" : ""
        case .kind: return Self.kind(t)
        case .bitRate: return t.bitRate > 0 ? "\(t.bitRate) kbps" : ""
        case .size: return ByteCountFormatter.string(fromByteCount: t.size, countStyle: .file)
        case .modified:
            return t.mtimeMS > 0 ? Self.dates.string(from: Date(timeIntervalSince1970: Double(t.mtimeMS) / 1000)) : ""
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

/// Which Songs table columns show, their order and the widths the user
/// set, kept in the user's defaults. The header's menu, View > Columns and
/// header drags change it.
@MainActor
final class SongColumns: ObservableObject {
    /// Every column, shown or not, in display order.
    @Published private(set) var order: [SongColumn]
    /// The columns the user showed (true) or hid (false). The others
    /// follow visibleByDefault.
    @Published private(set) var visibility: [String: Bool]
    /// Raised by Restore Default Columns, so the table resets its widths.
    @Published private(set) var restores = 0
    /// Widths the user dragged, by column id. Not published: only the
    /// table sets and reads them.
    private(set) var widths: [String: Double]

    private struct Saved: Codable {
        var order: [String]
        var visibility: [String: Bool]
        var widths: [String: Double]
    }

    private let key: String
    private var saveScheduled = false

    init(key: String = "SongsTable.layout") {
        self.key = key
        var saved: Saved?
        if let data = UserDefaults.standard.data(forKey: key) {
            saved = try? JSONDecoder().decode(Saved.self, from: data)
        }
        order = SongColumns.normalized(saved?.order ?? [])
        visibility = saved?.visibility ?? SongColumns.oldVisibility()
        widths = saved?.widths ?? [:]
    }

    /// Known ids in the saved order, then any column added since.
    private static func normalized(_ ids: [String]) -> [SongColumn] {
        var out: [SongColumn] = []
        for id in ids {
            if let c = SongColumn(rawValue: id), !out.contains(c) { out.append(c) }
        }
        for c in SongColumn.allCases where !out.contains(c) { out.append(c) }
        return out
    }

    /// The shown and hidden columns saved by the SwiftUI table that this
    /// one replaced. Its column order can't be read back.
    private static func oldVisibility() -> [String: Bool] {
        guard let data = UserDefaults.standard.data(forKey: "SongsTable.columns"),
              let old = try? JSONDecoder().decode(TableColumnCustomization<Track>.self, from: data)
        else { return [:] }
        var out: [String: Bool] = [:]
        for c in SongColumn.allCases {
            switch old[visibility: c.id] {
            case .visible: out[c.id] = true
            case .hidden: out[c.id] = false
            default: break
            }
        }
        return out
    }

    static func isVisible(_ column: SongColumn, in visibility: [String: Bool]) -> Bool {
        !column.canHide || (visibility[column.id] ?? column.visibleByDefault)
    }

    func isVisible(_ column: SongColumn) -> Bool { Self.isVisible(column, in: visibility) }

    func setVisible(_ column: SongColumn, _ visible: Bool) {
        guard column.canHide, isVisible(column) != visible else { return }
        visibility[column.id] = visible
        save()
    }

    func setOrder(_ columns: [SongColumn]) {
        let new = SongColumns.normalized(columns.map(\.id))
        guard new != order else { return }
        order = new
        save()
    }

    func width(for column: SongColumn) -> CGFloat {
        widths[column.id].map { CGFloat($0) } ?? column.width.ideal
    }

    /// Saved a moment later: a header drag sets a width many times.
    func setWidth(_ width: CGFloat, for column: SongColumn) {
        widths[column.id] = Double(width)
        guard !saveScheduled else { return }
        saveScheduled = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { [weak self] in
            MainActor.assumeIsolated { self?.save() }
        }
    }

    func restoreDefaults() {
        order = SongColumn.allCases
        visibility = [:]
        widths = [:]
        restores += 1
        save()
    }

    private func save() {
        saveScheduled = false
        let saved = Saved(order: order.map(\.id), visibility: visibility, widths: widths)
        if let data = try? JSONEncoder().encode(saved) {
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
