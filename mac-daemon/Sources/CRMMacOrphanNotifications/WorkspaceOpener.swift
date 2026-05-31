// WorkspaceOpener — façade over NSWorkspace.shared.open(_:) so
// tap-handler tests can assert which URL was opened without
// actually opening any URL (app launch, Finder, or browser).
import Foundation
import AppKit

/// Async, Sendable façade over NSWorkspace.shared.open(_:). Returns
/// true when the OS accepted the open call; tests inject a
/// recording fake.
public protocol WorkspaceOpener: Sendable {
    func open(_ url: URL) async -> Bool
}

/// Production conformance — wraps NSWorkspace.shared.open(_:).
public struct NSWorkspaceOpener: WorkspaceOpener {
    public init() {}

    public func open(_ url: URL) async -> Bool {
        // NSWorkspace.shared is the main-thread singleton; the
        // call itself is documented thread-safe and non-blocking
        // (it dispatches the open to LaunchServices).
        NSWorkspace.shared.open(url)
    }
}
