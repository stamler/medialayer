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

    var connected = false
    var folder = ""
    var dbPath = ""
    var dbEpoch: Int64 = 0
    var artDir = ""
    var streamBase = ""
    var tracks: Int64 = 0
    var job = Job()
    var error: String?

    var isBusy: Bool { ["opening", "listing", "indexing", "publishing"].contains(job.phase) }
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

    static func listFolders(in dir: String) async throws -> [String] {
        struct Reply: Decodable { var folders: [String]?; var error: String? }
        return try await Task.detached {
            let json = dir.withCString { take(MLListFolders(UnsafeMutablePointer(mutating: $0))) } ?? "{}"
            let reply = try JSONDecoder().decode(Reply.self, from: Data(json.utf8))
            if let error = reply.error { throw CoreError(message: error) }
            return reply.folders ?? []
        }.value
    }

    static func setFolder(_ folder: String) throws {
        try check(folder.withCString { MLSetFolder(UnsafeMutablePointer(mutating: $0)) })
    }

    static func startSync() throws {
        try check(MLStartSync())
    }
}
