// BundleAssembler stages a crm-mac.app bundle into the filesystem via
// FilesystemAdapter + ExecutableAdapter. The shell-script equivalent
// at `Scripts/assemble_bundle.sh` covers the build-time path (no
// Swift adapters available there); BundleAssembler covers the
// install-time path inside the running daemon binary.
//
// Layout written by `assemble(_:)`:
//
//   <bundlePath>/Contents/
//     Info.plist                        <- input.infoPlistContent (raw bytes)
//     MacOS/crm-mac                     <- copy of input.machoSourcePath, chmod +x
//     Library/LaunchAgents/
//       xyz.spengrah.crm-mac.plist      <- input.launchAgentPlistContent (string)
//
// Two-pass codesign is delegated to
// `ExecutableAdapter.adhocCodesignBundle(bundlePath:identifier:)`.
//
// The caller is responsible for the tmp-path-then-rename pattern; this
// method writes directly to `bundlePath`. Failure recovery (rollback
// of a partial write) is the caller's job — every step is idempotent
// from the caller's perspective (re-running with the same input
// overwrites the partial output).
import Foundation

/// Inputs for `BundleAssembler.assemble(_:)`. All fields are
/// provided by the caller so this type doesn't take a dependency on
/// LaunchAgentPlist (whose render needs install-time paths the
/// assembler doesn't otherwise know).
public struct BundleAssemblerInput: Equatable {
    /// Source Mach-O to be staged into Contents/MacOS/. Typically
    /// the currently-running binary at Bundle.main.executablePath.
    public let machoSourcePath: String
    /// Final bundle path (e.g.
    /// ~/Library/Application Support/crm-mac/crm-mac.app).
    public let bundlePath: String
    /// LaunchAgents plist content. The caller renders it (typically
    /// via LaunchAgentPlist.render() with the final install-time
    /// bundle path embedded directly) and this method writes the
    /// string verbatim. The plist is sealed by the bundle codesign
    /// pass below, so the caller MUST embed the real install path
    /// before assemble() runs — modifying the plist after assemble()
    /// returns would invalidate the codesign seal and SMAppService
    /// would refuse to load the bundle.
    public let launchAgentPlistContent: String
    /// Info.plist content as raw bytes. The production caller passes
    /// `PropertyListSerialization.data(...)` output rendered from
    /// `Bundle.main.infoDictionary`; tests pass fixture bytes
    /// directly. The dict on the install-time path resolves from the
    /// Mach-O `__TEXT,__info_plist` section (fresh install) or
    /// `Contents/Info.plist` (upgrade running from a bundle).
    public let infoPlistContent: Data
    /// Codesign identifier for the inner Mach-O. Always
    /// `Daemon.label` in production; parameterized for tests.
    public let codesignIdentifier: String

    public init(
        machoSourcePath: String,
        bundlePath: String,
        launchAgentPlistContent: String,
        infoPlistContent: Data,
        codesignIdentifier: String
    ) {
        self.machoSourcePath = machoSourcePath
        self.bundlePath = bundlePath
        self.launchAgentPlistContent = launchAgentPlistContent
        self.infoPlistContent = infoPlistContent
        self.codesignIdentifier = codesignIdentifier
    }
}

public struct BundleAssembler {
    public let filesystem: FilesystemAdapter
    public let executable: ExecutableAdapter

    public init(filesystem: FilesystemAdapter, executable: ExecutableAdapter) {
        self.filesystem = filesystem
        self.executable = executable
    }

    /// LaunchAgent plist filename inside the bundle:
    /// `Contents/Library/LaunchAgents/xyz.spengrah.crm-mac.plist`.
    /// Matches `Daemon.label + ".plist"`.
    public static let launchAgentPlistFilename = "\(Daemon.label).plist"

    /// Subpath of Contents/Info.plist inside the bundle.
    public static let infoPlistRelativePath = "Contents/Info.plist"

    /// Subpath of Contents/MacOS/crm-mac inside the bundle.
    public static let machoRelativePath = "Contents/MacOS/crm-mac"

    /// Subpath of the embedded LaunchAgents plist inside the bundle.
    public static let launchAgentPlistRelativePath =
        "Contents/Library/LaunchAgents/\(Daemon.label).plist"

    /// Assemble the bundle. Writes directly to `input.bundlePath`.
    public func assemble(_ input: BundleAssemblerInput) throws {
        // 1. Create the directory skeleton (idempotent — createDirectory
        //    is `mkdir -p`).
        let contentsDir = "\(input.bundlePath)/Contents"
        let macOSDir = "\(contentsDir)/MacOS"
        let launchAgentsDir = "\(contentsDir)/Library/LaunchAgents"
        try filesystem.createDirectory(at: input.bundlePath)
        try filesystem.createDirectory(at: contentsDir)
        try filesystem.createDirectory(at: macOSDir)
        try filesystem.createDirectory(at: launchAgentsDir)

        // 2. Stage the Mach-O.
        let machoDest = "\(input.bundlePath)/\(Self.machoRelativePath)"
        try filesystem.copy(from: input.machoSourcePath, to: machoDest)
        try filesystem.makeExecutable(at: machoDest)

        // 3. Write Info.plist verbatim from the caller-provided bytes.
        let infoPlistDest = "\(input.bundlePath)/\(Self.infoPlistRelativePath)"
        try filesystem.write(input.infoPlistContent, to: infoPlistDest)

        // 4. Write the LaunchAgents plist verbatim from the
        //    caller-provided string. The caller must embed the final
        //    install-time bundle path before assemble() — the plist is
        //    a sealed resource under the codesign manifest written in
        //    step 5.
        let launchAgentDest =
            "\(input.bundlePath)/\(Self.launchAgentPlistRelativePath)"
        try filesystem.write(
            Data(input.launchAgentPlistContent.utf8),
            to: launchAgentDest)

        // 5. Two-pass ad-hoc codesign: inner Mach-O with
        //    an explicit `--identifier <bundle-id>` (so TCC keys on
        //    the bundle ID), then bundle seal. Wrapped in the
        //    ExecutableAdapter so tests can record + script the
        //    codesign invocation without shelling out.
        try executable.adhocCodesignBundle(
            bundlePath: input.bundlePath,
            identifier: input.codesignIdentifier)
    }
}
