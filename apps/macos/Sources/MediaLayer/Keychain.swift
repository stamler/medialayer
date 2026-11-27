import Foundation
import Security

/// The key that encrypts the core's saved sign-ins (Google's tokens,
/// Proton's session) on disk. It lives in the login keychain, as Proton's
/// own apps keep their sessions, so the file in Application Support opens
/// nothing on its own. It isn't synced to other devices.
enum ConfigKey {
    private static let service = "MediaLayer"
    private static let account = "Saved sign-ins key"

    /// Returns the key, making and saving one the first time. Throws when
    /// the keychain can't be read (access denied, say): a new key then
    /// would lock the saved sign-ins away for good.
    static func load() throws -> String {
        var query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecSuccess {
            if let data = item as? Data, let key = String(data: data, encoding: .utf8), !key.isEmpty {
                return key
            }
            // An item with no usable key can't unlock anything: replace it.
            // The core then sets the old sign-ins aside.
            let deleted = SecItemDelete([
                kSecClass as String: kSecClassGenericPassword,
                kSecAttrService as String: service,
                kSecAttrAccount as String: account,
            ] as CFDictionary)
            guard deleted == errSecSuccess else { throw failure("replace", deleted) }
        } else if status != errSecItemNotFound {
            throw failure("read", status)
        }

        var bytes = [UInt8](repeating: 0, count: 32)
        let random = SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes)
        guard random == errSecSuccess else { throw failure("make", random) }
        let key = Data(bytes).base64EncodedString()
        query.removeValue(forKey: kSecReturnData as String)
        query.removeValue(forKey: kSecMatchLimit as String)
        query[kSecValueData as String] = Data(key.utf8)
        query[kSecAttrLabel as String] = "MediaLayer saved sign-ins"
        let added = SecItemAdd(query as CFDictionary, nil)
        guard added == errSecSuccess else { throw failure("save", added) }
        return key
    }

    private static func failure(_ what: String, _ status: OSStatus) -> CoreError {
        let reason = SecCopyErrorMessageString(status, nil) as String? ?? "error \(status)"
        return CoreError(message: "MediaLayer couldn't \(what) its key in the keychain (\(reason)). Quit and open it again, and allow access when macOS asks.")
    }
}

/// SMB passwords, one internet password item per server and user in the
/// login keychain, the kind Finder saves for "Connect to Server". The
/// core never sees the keychain: the app reads the passwords at launch
/// and hands them over (Core.setSMBPassword). Kept in this one type, so a
/// move to another keychain later changes only this.
enum SMBPassword {
    /// MediaLayer's items carry a path of their own: the label isn't
    /// part of an internet password's identity, so without it saving
    /// would collide with Finder's item for the same server and user.
    private static let path = "/MediaLayer"

    /// MediaLayer's items always name the port, so two ports on one
    /// server stay apart (a query without one matches any port). Finder
    /// saves port 445 as no port, so its lookup leaves it out then.
    private static func query(host: String, port: Int, user: String, finder: Bool = false) -> [String: Any] {
        var q: [String: Any] = [
            kSecClass as String: kSecClassInternetPassword,
            kSecAttrServer as String: host,
            kSecAttrAccount as String: user,
            kSecAttrProtocol as String: kSecAttrProtocolSMB,
        ]
        if !finder || port != 445 { q[kSecAttrPort as String] = port }
        return q
    }

    /// The saved password, or nil. With finder true it looks for any
    /// item for the server and user, such as Finder's (macOS asks the
    /// user whether MediaLayer may read it); otherwise only MediaLayer's.
    static func load(host: String, port: Int, user: String, finder: Bool = false) -> String? {
        var q = query(host: host, port: port, user: user, finder: finder)
        if !finder { q[kSecAttrPath as String] = path }
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &item) == errSecSuccess,
              let data = item as? Data, let p = String(data: data, encoding: .utf8), !p.isEmpty
        else { return nil }
        return p
    }

    /// Saves the password as MediaLayer's own item, replacing the one
    /// before.
    static func save(host: String, port: Int, user: String, domain: String, password: String) throws {
        var q = query(host: host, port: port, user: user)
        q[kSecAttrPath as String] = path
        SecItemDelete(q as CFDictionary) // only MediaLayer's, by its path
        q[kSecAttrLabel as String] = label(host: host, user: user)
        if !domain.isEmpty { q[kSecAttrSecurityDomain as String] = domain }
        q[kSecValueData as String] = Data(password.utf8)
        let status = SecItemAdd(q as CFDictionary, nil)
        guard status == errSecSuccess else {
            let reason = SecCopyErrorMessageString(status, nil) as String? ?? "error \(status)"
            throw CoreError(message: "MediaLayer couldn't save the password in the keychain (\(reason)).")
        }
    }

    private static func label(host: String, user: String) -> String { "MediaLayer: \(user) on \(host)" }
}
