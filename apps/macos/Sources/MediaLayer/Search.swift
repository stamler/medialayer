import AppKit
import Foundation
import SwiftUI

/// Filters the library by the toolbar's search text.
///
/// It works in memory over the tracks AppModel already holds. Each track
/// gets a search key once: its name, artist, album artist, album, composer
/// and genre, folded (no case, no diacritics) and joined with newlines, as
/// UTF-8 bytes. A keystroke is then one byte search per word per track,
/// with no allocation per track, which keeps a 10,000-track library well
/// within a frame. A SQL query would have to fold text the same way in
/// SQLite (which has no diacritic folding) and would reload every row on
/// each keystroke.
final class TrackSearch {
    private typealias Entry = (track: Track, key: [UInt8])
    private var entries: [Entry] = []
    private var stale = true

    /// Call when the tracks change; the keys are rebuilt on the next search.
    func invalidate() { stale = true }

    /// The tracks in which every word of `query` is part of some field.
    func filter(_ tracks: [Track], query: String) -> [Track] {
        let words = Self.words(query)
        guard !words.isEmpty else { return tracks }
        if stale { rebuild(tracks) }
        return entries.compactMap { entry in
            words.allSatisfy { Self.contains(entry.key, $0) } ? entry.track : nil
        }
    }

    /// Builds the keys, reusing them for tracks that didn't change, so a
    /// search left on while indexing doesn't fold the whole library again.
    private func rebuild(_ tracks: [Track]) {
        var old = [Track.ID: Entry](minimumCapacity: entries.count)
        for e in entries { old[e.track.id] = e }
        entries = tracks.map { t -> Entry in
            if let e = old[t.id], e.track == t { return e }
            return (t, Self.key(t))
        }
        stale = false
    }

    static func fold(_ s: String) -> String {
        s.folding(options: [.caseInsensitive, .diacriticInsensitive, .widthInsensitive], locale: nil)
            .precomposedStringWithCanonicalMapping
    }

    /// Fields are joined with a newline, which no word contains, so a word
    /// can't match across two fields.
    static func key(_ t: Track) -> [UInt8] {
        let fields = [t.displayTitle, t.artist, t.albumArtist, t.album, t.composer, t.genre]
        return Array(fold(fields.joined(separator: "\n")).utf8)
    }

    static func words(_ query: String) -> [[UInt8]] {
        fold(query).split(whereSeparator: { $0.isWhitespace || $0.isNewline }).map { Array($0.utf8) }
    }

    /// Whether `needle` occurs in `haystack`, byte for byte. UTF-8 never
    /// matches inside a multi-byte character, so this is a text match.
    static func contains(_ haystack: [UInt8], _ needle: [UInt8]) -> Bool {
        haystack.withUnsafeBufferPointer { h in
            needle.withUnsafeBufferPointer { n in
                guard let hp = h.baseAddress, let np = n.baseAddress else { return n.isEmpty }
                return memmem(hp, h.count, np, n.count) != nil
            }
        }
    }
}

/// The search field in the toolbar.
struct SearchToolbarItem: ToolbarContent {
    let width: CGFloat
    var body: some ToolbarContent {
        ToolbarItem(placement: .primaryAction) { SearchBox().frame(width: width) }
    }
}

private struct SearchBox: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var browse: BrowseSignal // searchText announces itself here

    var body: some View {
        SearchField(text: $model.searchText, focusRequest: model.searchFocusRequest)
            .help("Search name, artist, album, composer and genre")
    }
}

/// An NSSearchField, so that the field's editor is an NSText (AppModel's
/// Space key monitor leaves those alone), ⌘F can make it first responder
/// on macOS 14, and Escape clears it.
struct SearchField: NSViewRepresentable {
    @Binding var text: String
    /// Focuses the field each time it changes (Edit > Find, ⌘F).
    let focusRequest: Int

    func makeCoordinator() -> Coordinator { Coordinator(text: $text, focusRequest: focusRequest) }

    func makeNSView(context: Context) -> NSSearchField {
        let field = NSSearchField()
        field.placeholderString = "Search"
        field.stringValue = text
        field.sendsSearchStringImmediately = true // filter as you type
        field.sendsWholeSearchString = false
        field.delegate = context.coordinator
        field.target = context.coordinator
        field.action = #selector(Coordinator.changed(_:))
        return field
    }

    func updateNSView(_ field: NSSearchField, context: Context) {
        context.coordinator.text = $text
        if field.stringValue != text { field.stringValue = text }
        if focusRequest != context.coordinator.focusRequest {
            context.coordinator.focusRequest = focusRequest
            // After this update: making it first responder selects its text.
            DispatchQueue.main.async { field.window?.makeFirstResponder(field) }
        }
    }

    @MainActor
    final class Coordinator: NSObject, NSSearchFieldDelegate {
        var text: Binding<String>
        var focusRequest: Int

        init(text: Binding<String>, focusRequest: Int) {
            self.text = text
            self.focusRequest = focusRequest
        }

        /// The field's action: each keystroke, and the clear button.
        @objc func changed(_ sender: NSSearchField) { set(sender.stringValue) }

        func controlTextDidChange(_ note: Notification) {
            if let field = note.object as? NSSearchField { set(field.stringValue) }
        }

        /// Escape clears the field and the search.
        func control(_ control: NSControl, textView: NSTextView, doCommandBy selector: Selector) -> Bool {
            guard selector == #selector(NSResponder.cancelOperation(_:)), !control.stringValue.isEmpty else {
                return false
            }
            textView.string = ""
            control.stringValue = ""
            set("")
            return true
        }

        private func set(_ value: String) {
            if text.wrappedValue != value { text.wrappedValue = value }
        }
    }
}
