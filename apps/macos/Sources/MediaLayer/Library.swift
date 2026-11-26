import AppKit
import Foundation
import ImageIO
import SQLite3

/// One row of the Songs table. Text fields are "" when the file has no
/// such tag, so every column sorts.
struct Track: Identifiable, Hashable {
    let id: String
    let path: String
    let format: String
    let size: Int64
    let durationMS: Int64
    let sampleRate: Int
    let bitDepth: Int
    let title: String
    let artist: String
    let albumArtist: String
    let album: String
    let genre: String
    let composer: String
    let trackNumber: Int
    let discNumber: Int
    let year: String
    let artSHA: String? // hex SHA-256 of the cover, front cover first
    let mtimeMS: Int64 // the file's modification time on the primary

    /// The artist the Artists column groups by, as iTunes does.
    var browseArtist: String { albumArtist.isEmpty ? artist : albumArtist }
    var displayTitle: String { title.isEmpty ? (path as NSString).lastPathComponent : title }
    /// Average bit rate in kbit/s, from the file's size, so it includes
    /// tags and cover art.
    var bitRate: Int64 { durationMS > 0 ? size * 8 / durationMS : 0 }
}

/// Reads the index the Go core writes. The database is in WAL mode, so
/// reading here never blocks the indexer.
final class LibraryDB {
    private var db: OpaquePointer?

    init(path: String) throws {
        guard sqlite3_open_v2(path, &db, SQLITE_OPEN_READONLY | SQLITE_OPEN_FULLMUTEX, nil) == SQLITE_OK else {
            let message = db.map { String(cString: sqlite3_errmsg($0)) } ?? "cannot open"
            sqlite3_close(db)
            db = nil // deinit must not close it again
            throw CoreError(message: "open \(path): \(message)")
        }
        sqlite3_busy_timeout(db, 2000)
    }

    deinit { sqlite3_close(db) }

    private static let tagColumns = ["TITLE", "ARTIST", "ALBUMARTIST", "ALBUM", "GENRE", "COMPOSER",
                                     "TRACKNUMBER", "DISCNUMBER", "DATE"]

    func tracks() throws -> [Track] {
        let tags = Self.tagColumns.map {
            "(SELECT value FROM tag WHERE file_id = f.id AND key = '\($0)' ORDER BY seq LIMIT 1)"
        }.joined(separator: ", ")
        let sql = """
            SELECT f.id, f.path, f.format, f.size, f.duration_ms, f.sample_rate, f.bit_depth, \(tags),
              (SELECT hex(art_sha256) FROM file_art WHERE file_id = f.id ORDER BY role <> 3, role LIMIT 1),
              f.mtime
            FROM file f WHERE f.missing = 0
            """
        var stmt: OpaquePointer?
        guard sqlite3_prepare_v2(db, sql, -1, &stmt, nil) == SQLITE_OK else {
            throw CoreError(message: String(cString: sqlite3_errmsg(db)))
        }
        defer { sqlite3_finalize(stmt) }
        func text(_ i: Int32) -> String {
            sqlite3_column_text(stmt, i).map { String(cString: $0) } ?? ""
        }
        func leadingInt(_ s: String) -> Int { Int(s.prefix { $0.isNumber }) ?? 0 }
        var out: [Track] = []
        var rc = sqlite3_step(stmt)
        while rc == SQLITE_ROW {
            let art = text(16)
            out.append(Track(
                id: text(0), path: text(1), format: text(2),
                size: sqlite3_column_int64(stmt, 3), durationMS: sqlite3_column_int64(stmt, 4),
                sampleRate: Int(sqlite3_column_int(stmt, 5)), bitDepth: Int(sqlite3_column_int(stmt, 6)),
                title: text(7), artist: text(8), albumArtist: text(9), album: text(10),
                genre: text(11), composer: text(12),
                trackNumber: leadingInt(text(13)), discNumber: leadingInt(text(14)),
                year: String(text(15).prefix(4)), artSHA: art.isEmpty ? nil : art,
                mtimeMS: sqlite3_column_int64(stmt, 17)
            ))
            rc = sqlite3_step(stmt)
        }
        guard rc == SQLITE_DONE else { throw CoreError(message: String(cString: sqlite3_errmsg(db))) }
        return out
    }

    /// Where an image lives in the art packs.
    func artLocation(sha: String) -> (pack: String, offset: Int64, length: Int64)? {
        var stmt: OpaquePointer?
        guard let blob = Data(hex: sha),
              sqlite3_prepare_v2(db, "SELECT pack, pack_offset, pack_length FROM art WHERE sha256 = ?",
                                 -1, &stmt, nil) == SQLITE_OK else { return nil }
        defer { sqlite3_finalize(stmt) }
        let transient = unsafeBitCast(-1, to: sqlite3_destructor_type.self)
        _ = blob.withUnsafeBytes { sqlite3_bind_blob(stmt, 1, $0.baseAddress, Int32(blob.count), transient) }
        guard sqlite3_step(stmt) == SQLITE_ROW, let pack = sqlite3_column_text(stmt, 0) else { return nil }
        return (String(cString: pack), sqlite3_column_int64(stmt, 1), sqlite3_column_int64(stmt, 2))
    }
}

/// Loads covers from the art packs and keeps small thumbnails in memory.
/// The cache is bounded by the bytes of its decoded images. Requests for
/// one image at one size share a single decode, and a request nobody
/// waits for any more (its row scrolled away) is cancelled before it runs.
@MainActor
final class ArtCache {
    private let cache: NSCache<NSString, NSImage> = {
        let c = NSCache<NSString, NSImage>()
        c.totalCostLimit = 32 << 20 // decoded bytes; a 36-pixel thumbnail is about 5 KB
        c.countLimit = 4000
        return c
    }()
    private let queue: OperationQueue = {
        let q = OperationQueue()
        q.maxConcurrentOperationCount = 4
        q.qualityOfService = .userInitiated
        return q
    }()

    /// A decode under way and the callers waiting for it.
    private struct Request {
        let op: Operation
        var waiters: [Int: CheckedContinuation<NSImage?, Never>]
    }
    private var requests: [NSString: Request] = [:]
    private var nextWaiter = 0

    var db: LibraryDB?
    var artDir = ""

    private static func key(_ sha: String, _ size: Int) -> NSString { "\(sha)-\(size)" as NSString }

    func cached(_ sha: String, size: Int) -> NSImage? {
        cache.object(forKey: Self.key(sha, size))
    }

    /// A thumbnail at most `size` pixels on its long side, or nil if there
    /// is none or the calling task was cancelled.
    func image(_ sha: String, size: Int) async -> NSImage? {
        let key = Self.key(sha, size)
        if let hit = cache.object(forKey: key) { return hit }
        guard let db, !artDir.isEmpty else { return nil }
        let dir = artDir
        let waiter = nextWaiter
        nextWaiter += 1
        return await withTaskCancellationHandler {
            await withCheckedContinuation { (done: CheckedContinuation<NSImage?, Never>) in
                if Task.isCancelled { return done.resume(returning: nil) }
                if requests[key] != nil {
                    requests[key]!.waiters[waiter] = done
                    return
                }
                let op = BlockOperation()
                op.addExecutionBlock { [cache, weak op] in
                    guard let op, !op.isCancelled else { return }
                    let image = Self.decode(db: db, dir: dir, sha: sha, size: size)
                    if let image { cache.setObject(image, forKey: key, cost: size * size * 4) } // at most
                    DispatchQueue.main.async {
                        MainActor.assumeIsolated { self.finish(key, op: op, image: image) }
                    }
                }
                requests[key] = Request(op: op, waiters: [waiter: done])
                queue.addOperation(op)
            }
        } onCancel: {
            Task { @MainActor in self.cancel(key, waiter: waiter) }
        }
    }

    private func finish(_ key: NSString, op: Operation, image: NSImage?) {
        guard let r = requests[key], r.op === op else { return } // cancelled, or replaced since
        requests[key] = nil
        for done in r.waiters.values { done.resume(returning: image) }
    }

    private func cancel(_ key: NSString, waiter: Int) {
        guard var r = requests[key], let done = r.waiters.removeValue(forKey: waiter) else { return }
        done.resume(returning: nil)
        if r.waiters.isEmpty {
            r.op.cancel()
            requests[key] = nil
        } else {
            requests[key] = r
        }
    }

    nonisolated private static func decode(db: LibraryDB, dir: String, sha: String, size: Int) -> NSImage? {
        guard let loc = db.artLocation(sha: sha),
              let fh = FileHandle(forReadingAtPath: (dir as NSString).appendingPathComponent(loc.pack))
        else { return nil }
        defer { try? fh.close() }
        try? fh.seek(toOffset: UInt64(loc.offset))
        guard let data = try? fh.read(upToCount: Int(loc.length)),
              let src = CGImageSourceCreateWithData(data as CFData, nil),
              let cg = CGImageSourceCreateThumbnailAtIndex(src, 0, [
                  kCGImageSourceCreateThumbnailFromImageAlways: true,
                  kCGImageSourceThumbnailMaxPixelSize: size,
                  kCGImageSourceCreateThumbnailWithTransform: true,
              ] as CFDictionary)
        else { return nil }
        return NSImage(cgImage: cg, size: .zero)
    }
}

extension Data {
    /// Decodes a hex string, as SQLite's hex() writes it.
    init?(hex: String) {
        guard hex.count % 2 == 0 else { return nil }
        var bytes = [UInt8]()
        bytes.reserveCapacity(hex.count / 2)
        var it = hex.makeIterator()
        while let a = it.next(), let b = it.next() {
            guard let byte = UInt8(String([a, b]), radix: 16) else { return nil }
            bytes.append(byte)
        }
        self.init(bytes)
    }
}
