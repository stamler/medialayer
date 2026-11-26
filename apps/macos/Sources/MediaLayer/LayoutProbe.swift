import AppKit

/// MEDIALAYER_LAYOUT_PROBE=dir: a debugging aid for the window layout.
/// Once the library shows, it resizes the window to each width in
/// MEDIALAYER_LAYOUT_WIDTHS (default 1200,1000,900,840,800,700), as a
/// user drag would (never below the window's minimum), and for each writes
/// a picture of the window to dir/w<width>.png and a description of its
/// toolbar and split view to dir/layout.txt. Given more than one width, it
/// then toggles the sidebar, does the same again (w<width>-toggled.png),
/// and toggles it back; one width only resizes. It
/// needs no screen-recording permission: the window draws itself into the
/// picture. Materials don't draw that way, so the picture shows geometry,
/// not the real look.
enum LayoutProbe {
    private static var started = false

    @MainActor static func startIfRequested(toggleSidebar: @escaping @MainActor () -> Void) {
        let env = ProcessInfo.processInfo.environment
        guard !started, let dir = env["MEDIALAYER_LAYOUT_PROBE"], !dir.isEmpty else { return }
        started = true
        let widths = (env["MEDIALAYER_LAYOUT_WIDTHS"] ?? "1200,1000,900,840,800,700")
            .split(separator: ",").compactMap { Double($0.trimmingCharacters(in: .whitespaces)) }
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        Task { @MainActor in
            try? await Task.sleep(nanoseconds: 3_000_000_000)
            guard let window = NSApp.windows.first(where: { $0.isVisible && !($0 is NSPanel) && $0.toolbar != nil })
                ?? NSApp.windows.first(where: { $0.isVisible }) else { return }
            var report = ""
            // Each width with the sidebar as it is, then with it toggled
            // (through the app's own state, which the toolbar button also updates).
            // A single width only resizes.
            for toggled in widths.count > 1 ? [false, true] : [false] {
                if toggled {
                    toggleSidebar()
                    try? await Task.sleep(nanoseconds: 1_000_000_000)
                    report += "######## sidebar toggled\n"
                }
                for w in widths {
                    var frame = window.frame
                    let contentMin = window.frameRect(forContentRect: NSRect(origin: .zero, size: window.contentMinSize)).width
                    frame.size.width = max(w, window.minSize.width, contentMin)
                    window.setFrame(frame, display: true)
                    window.layoutIfNeeded()
                    try? await Task.sleep(nanoseconds: 800_000_000)
                    report += describe(window, requested: w)
                    snapshot(window, to: "\(dir)/w\(Int(w))\(toggled ? "-toggled" : "").png")
                }
            }
            if widths.count > 1 { toggleSidebar() } // back as it was
            try? report.write(toFile: "\(dir)/layout.txt", atomically: true, encoding: .utf8)
        }
    }

    @MainActor private static func describe(_ window: NSWindow, requested: Double) -> String {
        var s = "== requested \(Int(requested)) frame \(window.frame) minSize \(window.minSize) contentMinSize \(window.contentMinSize) contentLayoutRect \(window.contentLayoutRect)\n"
        if let toolbar = window.toolbar {
            let visible = Set((toolbar.visibleItems ?? []).map(\.itemIdentifier))
            s += "toolbar items \(toolbar.items.count), visible \(visible.count)\n"
            for item in toolbar.items {
                let v = item.view
                s += "  \(visible.contains(item.itemIdentifier) ? "shown " : "HIDDEN") \(item.itemIdentifier.rawValue.prefix(60)) view \(v.map { "\(type(of: $0)) \($0.frame) fit \($0.fittingSize)" } ?? "-")\n"
            }
        }
        if let content = window.contentView {
            s += "content \(content.frame) fitting \(content.fittingSize)\n"
            walk(content, depth: 0, into: &s)
        }
        return s
    }

    /// Split views and their panes, a few levels down.
    @MainActor private static func walk(_ view: NSView, depth: Int, into s: inout String) {
        guard depth < 14 else { return }
        if let split = view as? NSSplitView {
            s += String(repeating: "  ", count: depth) + "\(type(of: split)) vertical=\(split.isVertical) \(split.frame) in window \(split.convert(split.bounds, to: nil))\n"
            for pane in split.arrangedSubviews {
                s += String(repeating: "  ", count: depth + 1) + "pane \(type(of: pane)) \(pane.frame) hidden=\(pane.isHidden) fitting \(pane.fittingSize)\n"
            }
        }
        for sub in view.subviews { walk(sub, depth: depth + 1, into: &s) }
    }

    @MainActor private static func snapshot(_ window: NSWindow, to path: String) {
        guard let view = window.contentView?.superview ?? window.contentView,
              let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return }
        view.cacheDisplay(in: view.bounds, to: rep)
        try? rep.representation(using: .png, properties: [:])?.write(to: URL(fileURLWithPath: path))
    }
}
