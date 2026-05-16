// swift-tools-version:6.0
import PackageDescription

// Personal CRM Mac daemon (`crm-mac`).
//
// Single binary, two roles: a launchd-managed background agent and a
// user-facing CLI. See `../.ai/spec/mac-daemon.md` for the
// authoritative scope and target layering rationale.
//
// Target layering:
//   - CRMMacCore             (Foundation only): state, config, plugin protocol,
//                            StateMutator, SourceHealthSnapshot, PidfileLock,
//                            normalization parity helpers.
//   - CRMMacPiClient         (Foundation + CRMMacCore): typed HTTP client.
//   - CRMMacLifecycle        (Foundation + Core + PiClient): install/uninstall/
//                            doctor/status/heartbeat workflows + adapter protocols.
//                            NO system-framework imports — testable anywhere.
//   - CRMMacMessagesSource   (Foundation + Core + PiClient + GRDB): chat.db
//                            reader + messages source plugin. GRDB is isolated
//                            here so other targets stay Foundation-only.
//   - CRMMacSystem           (Foundation + os.log + Security + Core + Lifecycle):
//                            production impls of the Lifecycle adapter protocols.
//   - crm-mac                (executable): composition root; wires CRMMacSystem
//                            impls into CRMMacLifecycle workflows.
//
// GRDB.swift is pinned to .upToNextMinor(from: "7.0.0") — i.e. 7.0.x.
// GRDB 7.10+ declares `swift-tools-version:6.1` which exceeds our 6.0
// declaration, so a wider range would break resolution. Stay on 7.0.0
// until a follow-up bumps both pins together.
let package = Package(
    name: "crm-mac",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "crm-mac", targets: ["crm-mac"]),
    ],
    dependencies: [
        .package(url: "https://github.com/apple/swift-argument-parser",
                 from: "1.3.0"),
        .package(url: "https://github.com/groue/GRDB.swift",
                 .upToNextMinor(from: "7.0.0")),
    ],
    targets: [
        .executableTarget(
            name: "crm-mac",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                "CRMMacCore",
                "CRMMacPiClient",
                "CRMMacLifecycle",
                "CRMMacMessagesSource",
                "CRMMacSystem",
            ]),
        .target(name: "CRMMacCore"),
        .target(
            name: "CRMMacPiClient",
            dependencies: ["CRMMacCore"]),
        .target(
            name: "CRMMacLifecycle",
            dependencies: ["CRMMacCore", "CRMMacPiClient"]),
        .target(
            name: "CRMMacMessagesSource",
            dependencies: [
                "CRMMacCore",
                "CRMMacPiClient",
                .product(name: "GRDB", package: "GRDB.swift"),
            ]),
        .target(
            name: "CRMMacSystem",
            dependencies: ["CRMMacCore", "CRMMacLifecycle"]),
        .testTarget(name: "CRMMacCoreTests",
                    dependencies: ["CRMMacCore"]),
        .testTarget(name: "CRMMacPiClientTests",
                    dependencies: ["CRMMacPiClient"],
                    resources: [.copy("Fixtures")]),
        .testTarget(name: "CRMMacSystemTests",
                    dependencies: ["CRMMacSystem"]),
        // CRMMacPiClient is an explicit test dep because SwiftPM does
        // not propagate transitive deps to test targets — the lifecycle
        // tests use URLProtocol-mocked PiClient to drive Installer /
        // HeartbeatLoop.
        .testTarget(name: "CRMMacLifecycleTests",
                    dependencies: ["CRMMacLifecycle", "CRMMacPiClient"]),
        .testTarget(name: "CRMMacMessagesSourceTests",
                    dependencies: ["CRMMacMessagesSource"],
                    resources: [.copy("Fixtures")]),
    ]
)
