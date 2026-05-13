// `crm-mac configure` is a deliberate stub. The iCloud Contacts
// container allowlist picker is the future home of this command;
// until then `crm-mac install` covers all required configuration.
import ArgumentParser

struct ConfigureCommand: ParsableCommand {
    static var configuration = CommandConfiguration(
        commandName: "configure",
        abstract: "Interactive configuration (not implemented in this version).")

    mutating func run() throws {
        print("crm-mac configure: not implemented in this version.")
        print("For now, `crm-mac install` covers all required configuration.")
    }
}
