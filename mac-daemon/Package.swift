// swift-tools-version:6.0
import PackageDescription

// Personal CRM Mac daemon (`crm-mac`).
//
// Single binary, two roles: a launchd-managed background agent and a
// user-facing CLI. See `../.ai/spec/mac-daemon.md` for the
// authoritative scope and target layering rationale.
//
// Target layering:
//   - CRMMacCore                 (Foundation only): state, config, plugin protocol,
//                                StateMutator, SourceHealthSnapshot, PidfileLock,
//                                normalization parity helpers, JCS canonicalizer,
//                                ContactRecord + ContainerInfo + ContainerKind,
//                                ContactsAuthorizationAdapter (Foundation-only
//                                protocol; production impl in CRMMacSystem).
//   - CRMMacPiClient             (Foundation + CRMMacCore): typed HTTP client.
//   - CRMMacLifecycle            (Foundation + Core + PiClient): install/uninstall/
//                                doctor/status/heartbeat workflows + adapter
//                                protocols. NO system-framework imports — testable
//                                anywhere.
//   - CRMMacMessagesSource       (Foundation + Core + PiClient + GRDB): chat.db
//                                reader + messages source plugin. GRDB is isolated
//                                here so other targets stay Foundation-only.
//   - CRMMacPhoneCallsSource     (Foundation + Core + PiClient + GRDB):
//                                CallHistoryDB reader + phone_calls source
//                                plugin. Shares the KnownIdentifiersCache
//                                in CRMMacCore with CRMMacMessagesSource.
//   - CRMMacIcloudContactsSource (Foundation + Contacts + Core + PiClient):
//                                CNContactStore reader + icloud_contacts source
//                                plugin. Contacts framework is isolated here.
//   - CRMMacOrphanNotifications  (Foundation + UserNotifications + AppKit + Core
//                                + PiClient): macOS notification center actor +
//                                pending-notification persistence + 5-min
//                                reconcile loop. UserNotifications + AppKit
//                                imports are isolated here so the rest of the
//                                daemon stays Foundation-only.
//   - CRMMacSystem               (Foundation + os.log + Security + Contacts + Core
//                                + Lifecycle): production impls of the Lifecycle
//                                adapter protocols, including the production
//                                ContactsAuthorizationAdapter +
//                                ContactContainerEnumerator.
//   - crm-mac                    (executable): composition root; wires CRMMacSystem
//                                impls into CRMMacLifecycle workflows. Embeds
//                                NSContactsUsageDescription via Info.plist
//                                injection into the Mach-O __TEXT,__info_plist
//                                section.
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
                "CRMMacPhoneCallsSource",
                "CRMMacIcloudContactsSource",
                "CRMMacAnarlogSource",
                "CRMMacOrphanNotifications",
                "CRMMacSystem",
            ],
            // Info.plist is embedded into the binary via linker
            // -sectcreate; it must NOT be copied into the Sources/
            // bundle resources (SPM would warn + try to ship it).
            exclude: ["Info.plist"],
            // Embed NSContactsUsageDescription into the executable's
            // Mach-O __TEXT,__info_plist section so the Contacts
            // framework can surface the consent prompt on first
            // requestAccess. SPM has no first-class Info.plist
            // support for command-line executables; this linker-
            // section approach is the documented workaround. The CI
            // smoke step verifies the section is present via
            // `otool -s __TEXT __info_plist`.
            linkerSettings: [
                .unsafeFlags(
                    [
                        "-Xlinker", "-sectcreate",
                        "-Xlinker", "__TEXT",
                        "-Xlinker", "__info_plist",
                        "-Xlinker", "Sources/crm-mac/Info.plist",
                    ],
                    .when(platforms: [.macOS])),
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
            name: "CRMMacPhoneCallsSource",
            dependencies: [
                "CRMMacCore",
                "CRMMacPiClient",
                .product(name: "GRDB", package: "GRDB.swift"),
            ]),
        .target(
            name: "CRMMacIcloudContactsSource",
            dependencies: [
                "CRMMacCore",
                "CRMMacPiClient",
            ]),
        .target(
            name: "CRMMacAnarlogSource",
            dependencies: [
                "CRMMacCore",
                "CRMMacPiClient",
                // The sessions plugin forwards needs_attention items
                // to OrphanNotificationCenter after every batch. The
                // OPPOSITE direction (notifications → anarlog) is
                // forbidden — OrphanNotificationCenter consumes a
                // narrow SessionMetadataLookup protocol whose
                // concrete adapter lives in this target.
                "CRMMacOrphanNotifications",
            ]),
        .target(
            name: "CRMMacOrphanNotifications",
            dependencies: [
                "CRMMacCore",
                "CRMMacPiClient",
            ]),
        .target(
            name: "CRMMacSystem",
            dependencies: ["CRMMacCore", "CRMMacLifecycle"],
            // FSEvents lives in CoreServices. Only the anarlog
            // sessions watcher uses it; isolating the framework link
            // here keeps the rest of the daemon Foundation-only.
            linkerSettings: [
                .linkedFramework("CoreServices",
                                 .when(platforms: [.macOS])),
            ]),
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
        .testTarget(name: "CRMMacPhoneCallsSourceTests",
                    dependencies: ["CRMMacPhoneCallsSource"],
                    resources: [.copy("Fixtures")]),
        .testTarget(name: "CRMMacIcloudContactsSourceTests",
                    dependencies: ["CRMMacIcloudContactsSource", "CRMMacPiClient"],
                    resources: [.copy("Fixtures")]),
        .testTarget(name: "CRMMacAnarlogSourceTests",
                    dependencies: ["CRMMacAnarlogSource", "CRMMacPiClient",
                                   "CRMMacOrphanNotifications"]),
        .testTarget(name: "CRMMacOrphanNotificationsTests",
                    dependencies: ["CRMMacOrphanNotifications", "CRMMacPiClient"]),
    ]
)
