// ContainerPicker is the pure-logic half of the install / configure
// container selection UI. The interactive shell (in
// `crm-mac install` / `crm-mac configure containers`) prints the
// rendered string to stdout, reads a line of stdin, and feeds it back
// into `parse(input:containers:)`.
//
// Defaults:
//   - .local                                            → include
//   - .cardDAV AND name.lowercased() == "icloud"        → include
//   - All other .cardDAV (Google, Yahoo, third-party)   → exclude
//   - .exchange                                         → exclude
//   - .unassigned                                       → exclude
//   - .unknown                                          → exclude (fail-closed)
//
// The `all` keyword is NOT accepted: it would conflict with the
// fail-closed-for-unknown-containers requirement (a future CardDAV
// provider would default to excluded but `all` would silently
// include it).
import Foundation
import CRMMacCore

public enum ContainerPickerError: Error, Equatable, CustomStringConvertible {
    case invalidInput(reason: String)
    case noContainers

    public var description: String {
        switch self {
        case .invalidInput(let reason):
            return "container picker: invalid input — \(reason)"
        case .noContainers:
            return "container picker: no Contacts containers visible"
        }
    }
}

public enum ContainerPicker {

    /// Compute the default-include set for the given containers.
    /// Defaults are computed by `CNContainer.containerType` ONLY plus
    /// a single, well-defined exact-name match for iCloud.
    public static func defaults(for containers: [ContainerInfo]) -> [String] {
        var out: [String] = []
        for c in containers {
            switch c.type {
            case .local:
                out.append(c.identifier)
            case .cardDAV:
                if c.name.lowercased() == "icloud" {
                    out.append(c.identifier)
                }
            case .exchange, .unassigned, .unknown:
                continue
            }
        }
        return out
    }

    /// Render the interactive picker prompt: a numbered list with
    /// `[default]` markers next to recommended containers AND a
    /// short hint for containers covered by other providers
    /// (CardDAV containers with Google/Yahoo names — a UX nudge,
    /// NOT a runtime gate).
    ///
    /// The output ends with a blank line + the operator instruction
    /// line. The caller appends a `\n` and reads input from stdin.
    public static func render(_ containers: [ContainerInfo]) -> String {
        if containers.isEmpty {
            return "No iCloud Contacts containers visible.\n"
        }
        var lines: [String] = ["Available iCloud Contacts containers:"]
        for (i, c) in containers.enumerated() {
            let marker = c.defaultIncluded ? " [default]" : suggestion(for: c)
            lines.append(
                "  \(i + 1). \(c.name) (\(c.identifier))\(marker)")
        }
        lines.append("")
        lines.append("Enter comma-separated numbers, or press Enter for defaults:")
        return lines.joined(separator: "\n") + "\n"
    }

    /// Parse an operator-supplied input line into a list of selected
    /// container identifiers, in the order the operator picked them.
    /// Empty input → return the default-include set.
    public static func parse(
        input rawInput: String,
        containers: [ContainerInfo]
    ) throws -> [String] {
        if containers.isEmpty {
            throw ContainerPickerError.noContainers
        }
        let trimmed = rawInput.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty {
            return defaults(for: containers)
        }
        // Reject the `all` keyword explicitly: a future CardDAV
        // provider would default to excluded under fail-closed
        // policy but `all` would silently include it, leaving the
        // operator in a wrong-allowlist state.
        if trimmed.lowercased() == "all" {
            throw ContainerPickerError.invalidInput(
                reason: "the 'all' keyword is not supported; enter explicit comma-separated numbers")
        }

        let parts = trimmed.split(separator: ",").map {
            $0.trimmingCharacters(in: .whitespaces)
        }
        var picked: [String] = []
        var seen: Set<Int> = []
        for piece in parts {
            guard let index = Int(piece) else {
                throw ContainerPickerError.invalidInput(
                    reason: "\(piece) is not a number")
            }
            guard index >= 1, index <= containers.count else {
                throw ContainerPickerError.invalidInput(
                    reason: "index \(index) out of range 1...\(containers.count)")
            }
            if seen.contains(index) {
                throw ContainerPickerError.invalidInput(
                    reason: "index \(index) appears more than once")
            }
            seen.insert(index)
            picked.append(containers[index - 1].identifier)
        }
        return picked
    }

    // MARK: - private

    /// Hint string for non-default containers (e.g. Google CardDAV
    /// covered by the Pi-side gcontacts provider). Empty string when
    /// no hint applies.
    private static func suggestion(for c: ContainerInfo) -> String {
        switch c.type {
        case .cardDAV:
            // Heuristic: well-known third-party CardDAV providers
            // typically managed by other Pi-side integrations.
            let lower = c.name.lowercased()
            if lower.contains("google") || lower.contains("@gmail") {
                return " [skipped — likely covered by gcontacts]"
            }
            return ""
        case .exchange:
            return " [skipped — Exchange]"
        case .unassigned, .unknown:
            return " [skipped — fail-closed default]"
        case .local:
            return ""
        }
    }
}
