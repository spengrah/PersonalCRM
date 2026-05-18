// `crm-mac configure` hosts subcommands that mutate non-pair
// configuration. Currently: `containers` for the iCloud Contacts
// CNContainer allowlist. Future per-source configs land as sibling
// subcommands.
//
// Allowlist-mutation flow:
//   1. Enumerate current CNContainers.
//   2. Run the picker (or accept --containers <id1>,<id2>).
//   3. Diff against the existing allowlist.
//   4. On non-empty diff: prompt [y/N]; on yes:
//      4a. Write the recovery flag into state.json FIRST.
//      4b. Write the updated config.json SECOND.
//      4c. DO NOT reset cursor; DO NOT clear hash cache. The
//          recovery flow consumes both.
//
// The state-then-config ordering is the crash-safety contract: a
// crash between 4a and 4b leaves the daemon recovering against the
// OLD allowlist on the next tick — still correct (tombstones
// removed-upstream contacts, emits no spurious wrong-allowlist
// contacts). A second `configure containers` run reapplies and
// completes the swap; idempotent. The reversed order would produce
// the wrong-allowlist + no-recovery state the recovery flow is
// designed to avoid.
import Foundation
import ArgumentParser
import CRMMacCore
import CRMMacIcloudContactsSource
import CRMMacLifecycle

struct ConfigureCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "configure",
        abstract: "Interactive configuration (per-source subcommands).",
        subcommands: [ContainersSubcommand.self])

    mutating func run() throws {
        print("crm-mac configure: pass a subcommand.")
        print("  containers   Manage the iCloud Contacts allowlist.")
        print("")
        print("Run `crm-mac configure containers --help` for details.")
    }
}

struct ContainersSubcommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "containers",
        abstract: "Pick or update the iCloud Contacts CNContainer allowlist.")

    @Flag(name: .long, help: "Print the current CNContainer list in script-consumable TSV format; no prompts.")
    var list: Bool = false

    @Option(
        name: .long,
        help: "Non-interactive: comma-separated CNContainer identifiers (UUIDs).")
    var containers: String = ""

    mutating func run() throws {
        let ctx = ProductionContext()

        // Refuse to run while the daemon is up so the allowlist
        // mutation doesn't race against an in-flight tick.
        try requireDaemonNotRunning(paths: ctx.paths)

        let enumerator = ctx.contactContainerEnumerator()
        let visible: [ContainerInfo]
        do {
            visible = try enumerator.listContainers()
        } catch ContactContainerEnumeratorError.notAuthorized {
            FileHandle.standardError.write(Data(
                "Contacts permission missing. Run `crm-mac install --re-request-permission` to grant.\n".utf8))
            throw ExitCode(1)
        } catch {
            FileHandle.standardError.write(Data(
                "container enumeration failed: \(error)\n".utf8))
            throw ExitCode(1)
        }

        if list {
            for c in visible {
                let typeLabel = Self.label(for: c.type)
                let defaultFlag = c.defaultIncluded ? "default" : "skip"
                print("\(c.identifier)\t\(typeLabel)\t\(c.name)\t\(defaultFlag)")
            }
            return
        }

        // Resolve the picked identifiers (interactive picker OR
        // --containers flag).
        let pickedIDs: [String]
        if !containers.isEmpty {
            pickedIDs = try Self.resolveNonInteractive(
                raw: containers, visible: visible)
        } else {
            FileHandle.standardOutput.write(Data(
                ContainerPicker.render(visible).utf8))
            let line = readLine() ?? ""
            do {
                pickedIDs = try ContainerPicker.parse(input: line, containers: visible)
            } catch {
                FileHandle.standardError.write(Data(
                    "\(error)\n".utf8))
                throw ExitCode(2)
            }
        }

        // Diff against the existing allowlist.
        let configStore = ConfigStore(fileURL: URL(fileURLWithPath: ctx.paths.configFilePath))
        let existingAllowlist = (try? configStore.loadICloudContactsConfig()?.containers) ?? []
        let existing = Set(existingAllowlist)
        let picked = Set(pickedIDs)
        let added = picked.subtracting(existing)
        let removed = existing.subtracting(picked)
        if added.isEmpty && removed.isEmpty {
            print("No allowlist changes detected.")
            return
        }

        print("Planned allowlist changes:")
        if !added.isEmpty {
            print("  Added:")
            for id in added.sorted() { print("    + \(id)") }
        }
        if !removed.isEmpty {
            print("  Removed:")
            for id in removed.sorted() { print("    - \(id)") }
        }
        print("")
        print("On confirm, the icloud_contacts source will perform a full /known-ids")
        print("reconciliation on its next tick: contacts removed from the allowlist")
        print("get tombstoned on the Pi; contacts added to it get upserted.")
        print("The local hash cache is preserved so deletes carry deterministic")
        print("prior hashes.")
        print("")
        print("Continue? [y/N] ", terminator: "")
        let answer = (readLine() ?? "").lowercased()
        if answer != "y" && answer != "yes" {
            print("Aborted; no changes written.")
            return
        }

        // Crash-safety ordering: state-write FIRST, config-write
        // SECOND. A crash between the two leaves the daemon
        // recovering against the OLD allowlist on its next tick —
        // still correct (tombstones removed-upstream contacts). A
        // second run reapplies the picker output and completes the
        // swap; idempotent.
        let stateStore = StateStore(fileURL: URL(fileURLWithPath: ctx.paths.stateFilePath))
        let mutator = StateMutator(store: stateStore)
        do {
            try Self.runStateUpdate(mutator: mutator)
        } catch {
            FileHandle.standardError.write(Data(
                "Failed to set recovery flag in state.json: \(error)\n".utf8))
            throw ExitCode(1)
        }

        do {
            try configStore.saveICloudContactsConfig(
                ICloudContactsConfig(containers: pickedIDs))
        } catch {
            FileHandle.standardError.write(Data(
                "Failed to write config.json: \(error)\n  (Recovery flag is set; re-run `crm-mac configure containers` to retry.)\n".utf8))
            throw ExitCode(1)
        }

        print("Allowlist updated. Run `crm-mac status` to confirm the next tick performs the reconciliation.")
    }

    private static func runStateUpdate(mutator: StateMutator) throws {
        // Bridge async into the synchronous CLI run() flow. Pattern
        // mirrors MessagesCommand for the same reason.
        let sem = DispatchSemaphore(value: 0)
        nonisolated(unsafe) var mutateError: Error?
        Task {
            do {
                try await mutator.mutate { state in
                    var src = state.sources["icloud_contacts"] ?? SourceState()
                    src.lastError = "recovery_requested:allowlist_changed"
                    src.lastErrorAt = Date()
                    state.sources["icloud_contacts"] = src
                }
            } catch {
                mutateError = error
            }
            sem.signal()
        }
        sem.wait()
        if let e = mutateError { throw e }
    }

    private static func resolveNonInteractive(
        raw: String,
        visible: [ContainerInfo]
    ) throws -> [String] {
        let visibleIDs = Set(visible.map(\.identifier))
        let parts = raw.split(separator: ",").map {
            $0.trimmingCharacters(in: .whitespaces)
        }
        if parts.isEmpty {
            return ContainerPicker.defaults(for: visible)
        }
        for id in parts {
            if !visibleIDs.contains(id) {
                FileHandle.standardError.write(Data(
                    "container identifier \(id) is not in the visible CNContainer list. Run `crm-mac configure containers --list` to see available IDs.\n".utf8))
                throw ExitCode(2)
            }
        }
        return parts
    }

    private static func label(for kind: ContainerKind) -> String {
        switch kind {
        case .local: return "local"
        case .cardDAV: return "cardDAV"
        case .exchange: return "exchange"
        case .unassigned: return "unassigned"
        case .unknown(let raw): return "unknown(\(raw))"
        }
    }
}

/// Refuses to proceed when the daemon's pidfile is present.
private func requireDaemonNotRunning(paths: LifecyclePaths) throws {
    let pidPath = URL(fileURLWithPath: paths.runtimeDirPath)
        .appendingPathComponent("daemon.pid").path
    guard FileManager.default.fileExists(atPath: pidPath) else { return }
    FileHandle.standardError.write(Data(
        "crm-mac daemon appears to be running (pidfile at \(pidPath)). Stop it first: launchctl bootout gui/$(id -u) \(Daemon.label)\n".utf8))
    throw ExitCode(3)
}
