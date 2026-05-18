// ProductionContext wires the CRMMacSystem implementations into the
// CRMMacLifecycle workflow types. The executable is the only place
// that touches both halves; lifecycle tests use in-memory fakes
// instead.
import Foundation
import CRMMacCore
import CRMMacLifecycle
import CRMMacPiClient
import CRMMacSystem

struct ProductionContext {
    let paths: LifecyclePaths
    let logger: LoggerProtocol
    let filesystem: FilesystemAdapter
    let executable: ExecutableAdapter
    let keychain: KeychainStore
    let launchctl: LaunchctlRunner
    let clock: ClockAdapter
    let exitHandler: ExitHandler

    init() {
        let systemPaths = DaemonPaths()
        self.paths = LifecyclePaths(
            configDirPath: systemPaths.configDir.path,
            binDirPath: systemPaths.binDir.path,
            binaryPath: systemPaths.binaryPath.path,
            configFilePath: systemPaths.configFile.path,
            stateFilePath: systemPaths.stateFile.path,
            launchAgentsDirPath: systemPaths.launchAgentsDir.path,
            plistPath: systemPaths.plistPath.path,
            logsDirPath: systemPaths.logsDir.path,
            stdoutLogPath: systemPaths.stdoutLog.path,
            stderrLogPath: systemPaths.stderrLog.path)
        self.logger = OSLogLogger()
        self.filesystem = ProductionFilesystemAdapter()
        self.executable = ProductionExecutableAdapter()
        self.keychain = ProductionKeychainStore()
        self.launchctl = ProductionLaunchctlRunner()
        self.clock = SystemClock()
        self.exitHandler = SystemExitHandler()
    }

    func piClientFactory(_ url: URL) -> PiClient {
        PiClient(baseURL: url, logger: logger)
    }

    func installer() -> Installer {
        Installer(InstallerDependencies(
            paths: paths,
            filesystem: filesystem,
            executable: executable,
            keychain: keychain,
            launchctl: launchctl,
            piClientFactory: { url in self.piClientFactory(url) },
            clock: clock,
            logger: logger))
    }

    func uninstaller() -> Uninstaller {
        Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: filesystem,
            keychain: keychain,
            launchctl: launchctl,
            logger: logger))
    }

    func doctor() -> Doctor {
        Doctor(DoctorDependencies(
            paths: paths,
            filesystem: filesystem,
            keychain: keychain,
            launchctl: launchctl,
            piClientFactory: { url in self.piClientFactory(url) },
            contactsAuth: contactsAuthAdapter(),
            containerEnumerator: contactContainerEnumerator(),
            tickInterval: 15 * 60,
            clock: clock,
            logger: logger))
    }

    func status() -> Status {
        Status(StatusDependencies(
            paths: paths,
            filesystem: filesystem,
            launchctl: launchctl))
    }

    func contactsAuthAdapter() -> ContactsAuthorizationAdapter {
        ProductionContactsAuthorizationAdapter()
    }

    func contactContainerEnumerator() -> ContactContainerEnumerator {
        ProductionContactContainerEnumerator()
    }
}
