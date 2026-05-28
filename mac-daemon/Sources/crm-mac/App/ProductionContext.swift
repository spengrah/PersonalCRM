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
    let agentService: AgentService
    let processSignaller: ProcessSignaller
    let bundleAssembler: BundleAssembler
    /// Legacy launchctl runner — only used by the one-shot migration
    /// + uninstall's legacy-cleanup path. Stays wired even after the
    /// migration runs because uninstall can run more than once.
    let legacyLaunchctl: LaunchctlRunner
    /// Production launchctl runner — used by the re-pair flow's
    /// kickstart-restart step.
    let launchctl: LaunchctlRunner
    let clock: ClockAdapter
    let exitHandler: ExitHandler

    init() {
        let systemPaths = DaemonPaths()
        self.paths = LifecyclePaths(
            configDirPath: systemPaths.configDir.path,
            binDirPath: systemPaths.binDir.path,
            configFilePath: systemPaths.configFile.path,
            stateFilePath: systemPaths.stateFile.path,
            launchAgentsDirPath: systemPaths.launchAgentsDir.path,
            logsDirPath: systemPaths.logsDir.path,
            stdoutLogPath: systemPaths.stdoutLog.path,
            stderrLogPath: systemPaths.stderrLog.path,
            bundleAppPath: systemPaths.bundleApp.path,
            bundleBinaryPath: systemPaths.bundleBinary.path,
            bundlePlistPath: systemPaths.bundlePlist.path,
            bundleInfoPlistPath: systemPaths.bundleInfoPlist.path,
            legacyBinaryPath: systemPaths.legacyBinary.path,
            legacyPlistPath: systemPaths.legacyPlist.path)
        self.logger = OSLogLogger()
        let fs = ProductionFilesystemAdapter()
        let exec = ProductionExecutableAdapter()
        self.filesystem = fs
        self.executable = exec
        self.keychain = FileAPIKeyStore(path: systemPaths.apiKeyFile.path)
        if #available(macOS 13.0, *) {
            self.agentService = ProductionAgentService(
                plistName: "\(Daemon.label).plist")
        } else {
            fatalError("crm-mac requires macOS 13+ for SMAppService")
        }
        self.processSignaller = ProductionProcessSignaller()
        self.bundleAssembler = BundleAssembler(
            filesystem: fs, executable: exec)
        self.legacyLaunchctl = ProductionLaunchctlRunner()
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
            agentService: agentService,
            processSignaller: processSignaller,
            bundleAssembler: bundleAssembler,
            piClientFactory: { url in self.piClientFactory(url) },
            clock: clock,
            logger: logger,
            legacyKeychain: ProductionKeychainStore(),
            legacyLaunchctl: legacyLaunchctl))
    }

    func repairer() -> Repairer {
        // Capture the production logger by-value before forming the
        // @Sendable closure so it doesn't reach back into `self`
        // (ProductionContext is not Sendable; mirrors the pattern in
        // InstallCommand.runContactsPermissionFlow that constructs
        // fresh ProductionContext() instances inside @Sendable closures).
        let capturedLogger = logger
        return Repairer(RepairerDependencies(
            paths: paths,
            filesystem: filesystem,
            keychain: keychain,
            configStoreFactory: { url in ConfigStore(fileURL: url) },
            piClientFactory: { url in PiClient(baseURL: url, logger: capturedLogger) },
            launchctl: launchctl,
            logger: logger))
    }

    func uninstaller() -> Uninstaller {
        Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: filesystem,
            keychain: keychain,
            agentService: agentService,
            processSignaller: processSignaller,
            logger: logger,
            legacyLaunchctl: legacyLaunchctl))
    }

    func doctor() -> Doctor {
        Doctor(DoctorDependencies(
            paths: paths,
            filesystem: filesystem,
            keychain: keychain,
            agentService: agentService,
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
            agentService: agentService))
    }

    func contactsAuthAdapter() -> ContactsAuthorizationAdapter {
        ProductionContactsAuthorizationAdapter()
    }

    func contactContainerEnumerator() -> ContactContainerEnumerator {
        ProductionContactContainerEnumerator()
    }
}
