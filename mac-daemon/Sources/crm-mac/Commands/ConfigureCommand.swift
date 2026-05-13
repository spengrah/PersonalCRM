// `crm-mac configure` is a deliberate stub in PR6. PR8 fills in the
// iCloud container allowlist picker.
import ArgumentParser

struct ConfigureCommand: ParsableCommand {
    static var configuration = CommandConfiguration(
        commandName: "configure",
        abstract: "Interactive configuration (not implemented in this version; see PR8).")

    mutating func run() throws {
        print("crm-mac configure: not implemented in this version.")
        print("The iCloud Contacts container picker arrives in PR8 of the v1 roadmap.")
        print("For now, `crm-mac install` covers all required configuration.")
    }
}
