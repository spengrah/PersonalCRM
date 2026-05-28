// FakeWorkspaceOpener — recording fake for tests.
import Foundation
@testable import CRMMacOrphanNotifications

public actor FakeWorkspaceOpener: WorkspaceOpener {
    public private(set) var openedURLs: [URL] = []
    private var openResult: Bool

    public init(openResult: Bool = true) {
        self.openResult = openResult
    }

    public func setOpenResult(_ value: Bool) {
        self.openResult = value
    }

    public func recordedOpenedURLs() -> [URL] { openedURLs }

    public func open(_ url: URL) async -> Bool {
        openedURLs.append(url)
        return openResult
    }
}
