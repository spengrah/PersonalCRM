// DaemonStartup owns the daemon-mode boot sequence (config load ->
// keychain read -> state load) and maps each failure class to the
// canonical exit code.
//
// Extracted from DaemonCommand so the branchy startup paths (missing
// config / unreadable keychain / corrupt state) can be unit-tested
// without shelling out to the binary.
import Foundation
import CRMMacCore

/// Exit codes returned by the daemon mode. Matches the launchd
/// plist's KeepAlive policy: any non-zero non-crash exit stays dead
/// until the operator fixes the underlying issue and re-kickstarts.
public enum DaemonExitCode: Int32 {
    case clean = 0
    case authRevoked = 1
    case upgradeRequired = 2
    case configFailure = 3
    case keychainFailure = 4
    case stateFailure = 5
}

public enum DaemonStartupError: Error, Equatable, CustomStringConvertible {
    case config(String)
    case keychain(String)
    case state(String)

    public var description: String {
        switch self {
        case .config(let m): return "daemon: config load failed: \(m)"
        case .keychain(let m): return "daemon: keychain load failed: \(m)"
        case .state(let m): return "daemon: state load failed: \(m)"
        }
    }

    public var exitCode: DaemonExitCode {
        switch self {
        case .config: return .configFailure
        case .keychain: return .keychainFailure
        case .state: return .stateFailure
        }
    }
}

public struct DaemonStartupArtifacts {
    public let config: DaemonConfig
    public let apiKey: String
    public let stateStore: StateStore

    public init(config: DaemonConfig, apiKey: String, stateStore: StateStore) {
        self.config = config
        self.apiKey = apiKey
        self.stateStore = stateStore
    }
}

public struct DaemonStartup {
    public let paths: LifecyclePaths
    public let keychain: KeychainStore
    public let logger: LoggerProtocol
    public let stateStoreFactory: (URL) -> StateStore
    public let configStoreFactory: (URL) -> ConfigStore

    public init(
        paths: LifecyclePaths,
        keychain: KeychainStore,
        logger: LoggerProtocol,
        stateStoreFactory: @escaping (URL) -> StateStore = { StateStore(fileURL: $0) },
        configStoreFactory: @escaping (URL) -> ConfigStore = { ConfigStore(fileURL: $0) }
    ) {
        self.paths = paths
        self.keychain = keychain
        self.logger = logger
        self.stateStoreFactory = stateStoreFactory
        self.configStoreFactory = configStoreFactory
    }

    public func run() throws -> DaemonStartupArtifacts {
        let configStore = configStoreFactory(URL(fileURLWithPath: paths.configFilePath))
        let config: DaemonConfig
        do {
            config = try configStore.load()
        } catch {
            logger.error("daemon: config load failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            throw DaemonStartupError.config(String(describing: error))
        }

        let apiKey: String
        do {
            apiKey = try keychain.readAPIKey()
        } catch {
            logger.error("daemon: keychain load failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            throw DaemonStartupError.keychain(String(describing: error))
        }

        let stateStore = stateStoreFactory(URL(fileURLWithPath: paths.stateFilePath))
        do {
            _ = try stateStore.load()
        } catch {
            logger.error("daemon: state load failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            throw DaemonStartupError.state(String(describing: error))
        }

        return DaemonStartupArtifacts(
            config: config,
            apiKey: apiKey,
            stateStore: stateStore)
    }
}
