// swift-tools-version:6.0
import PackageDescription

// Personal CRM Mac daemon (`crm-mac`).
//
// Single binary, two roles: a launchd-managed background agent and a
// user-facing CLI. See `../.ai/spec/mac-daemon.md` for the
// authoritative scope and target layering rationale.
//
// Target layering:
//   - CRMMacCore      (Foundation only): state, config, plugin protocol.
//   - CRMMacPiClient  (Foundation + CRMMacCore): typed HTTP client.
//   - CRMMacLifecycle (Foundation + Core + PiClient): install/uninstall/
//                     doctor/status/heartbeat workflows + adapter protocols.
//                     NO system-framework imports — testable anywhere.
//   - CRMMacSystem    (Foundation + os.log + Security + Core + Lifecycle):
//                     production impls of the Lifecycle adapter protocols.
//   - crm-mac         (executable): composition root; wires CRMMacSystem
//                     impls into CRMMacLifecycle workflows.
let package = Package(
    name: "crm-mac",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "crm-mac", targets: ["crm-mac"]),
    ],
    dependencies: [
        .package(url: "https://github.com/apple/swift-argument-parser",
                 from: "1.3.0"),
    ],
    targets: [
        .executableTarget(
            name: "crm-mac",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                "CRMMacCore",
                "CRMMacPiClient",
                "CRMMacLifecycle",
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
    ]
)
