import CMLCore
import Foundation

/// The Go core's status, decoded from MLStatus's JSON.
struct CoreStatus: Decodable, Equatable {
    struct Job: Decodable, Equatable {
        var phase: String = "idle"
        var done: Int64 = 0
        var total: Int64 = 0
        var failed: Int64 = 0
        var current: String?
        var error: String?
        var failures: [String]?
        var finished: String? // when the last sync ended
    }

    /// One of the library's folders; the primary comes first.
    struct Source: Decodable, Equatable, Identifiable {
        var id: String
        var type: String // rclone backend: "drive", "local", ...
        var remote: String
        var folder: String
        var primary = false
        var state: String? // secondaries: syncing, synced, paused, error
        var copies = 0
        var replaces = 0
        var deletes = 0
        var error: String?
        var synced: String?

        var name: String { (folder as NSString).lastPathComponent }
    }

    var connected = false
    var folder = ""
    var dbPath = ""
    var dbEpoch: Int64 = 0
    var artDir = ""
    var streamBase = ""
    var tracks: Int64 = 0
    var job = Job()
    var sources: [Source]? = []
    var accounts: [String]? = []
    var notice: String?
    var error: String?

    var isBusy: Bool { ["opening", "listing", "indexing", "publishing", "mirroring"].contains(job.phase) }
}

struct CoreError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

/// A thin Swift face on the C functions in cmd/mlcore. Every string the
/// core returns is copied and freed here.
enum Core {
    /// Takes ownership of a string the core returned.
    private static func take(_ p: UnsafeMutablePointer<CChar>?) -> String? {
        guard let p else { return nil }
        defer { MLFree(p) }
        return String(cString: p)
    }

    private static func check(_ p: UnsafeMutablePointer<CChar>?) throws {
        if let message = take(p) { throw CoreError(message: message) }
    }

    static func start(dataDir: URL) throws {
        try check(dataDir.path.withCString { MLInit(UnsafeMutablePointer(mutating: $0)) })
    }

    static func status() -> CoreStatus {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        guard let json = take(MLStatus()), let data = json.data(using: .utf8),
              let status = try? decoder.decode(CoreStatus.self, from: data)
        else { return CoreStatus(error: "the core returned an unreadable status") }
        return status
    }

    /// Opens Google's sign-in page in the browser and waits for the user.
    static func connectGoogleDrive() async throws {
        try await Task.detached { try check(MLConnectGoogleDrive()) }.value
    }

    /// Signs in to another Google account for a secondary; returns its
    /// remote name.
    static func addGoogleAccount() async throws -> String {
        struct Reply: Decodable { var remote: String?; var error: String? }
        return try await Task.detached {
            let json = take(MLAddGoogleAccount()) ?? "{}"
            let reply = try JSONDecoder().decode(Reply.self, from: Data(json.utf8))
            if let error = reply.error { throw CoreError(message: error) }
            return reply.remote ?? ""
        }.value
    }

    /// Lists folders on a Google account ("" for the primary's).
    static func listFolders(remote: String = "", in dir: String) async throws -> [String] {
        struct Reply: Decodable { var folders: [String]?; var error: String? }
        return try await Task.detached {
            let json = remote.withCString { r in
                dir.withCString { d in
                    take(MLListFolders(UnsafeMutablePointer(mutating: r), UnsafeMutablePointer(mutating: d)))
                }
            } ?? "{}"
            let reply = try JSONDecoder().decode(Reply.self, from: Data(json.utf8))
            if let error = reply.error { throw CoreError(message: error) }
            return reply.folders ?? []
        }.value
    }

    static func setFolder(_ folder: String) throws {
        try check(folder.withCString { MLSetFolder(UnsafeMutablePointer(mutating: $0)) })
    }

    /// Adds a secondary: a folder on a Google account, or with an empty
    /// remote, a local folder.
    static func addSecondary(remote: String, folder: String) throws {
        try check(remote.withCString { r in
            folder.withCString { f in
                MLAddSecondary(UnsafeMutablePointer(mutating: r), UnsafeMutablePointer(mutating: f))
            }
        })
    }

    static func removeSecondary(_ id: String) throws {
        try check(id.withCString { MLRemoveSecondary(UnsafeMutablePointer(mutating: $0)) })
    }

    static func approveChanges(_ id: String) throws {
        try check(id.withCString { MLApproveChanges(UnsafeMutablePointer(mutating: $0)) })
    }

    static func promote(_ id: String) throws {
        try check(id.withCString { MLPromote(UnsafeMutablePointer(mutating: $0)) })
    }

    static func startSync() throws {
        try check(MLStartSync())
    }

    /// Looks up a track's cloud file in the background, ahead of playing it.
    static func prepare(_ fileID: String) {
        fileID.withCString { MLPrepare(UnsafeMutablePointer(mutating: $0)) }
    }
}
