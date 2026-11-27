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
