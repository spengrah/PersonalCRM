// Root ParsableCommand. Each subcommand defines its own argument
// surface; the discriminator is the first positional arg
// (`crm-mac daemon`, `crm-mac install`, …).
import ArgumentParser
import CRMMacLifecycle

@main
struct CRMMacCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "crm-mac",
        abstract: "Personal CRM Mac daemon — Apple Messages + iCloud Contacts ingestion.",
        version: Daemon.version,
        subcommands: [
            DaemonCommand.self,
            InstallCommand.self,
            UninstallCommand.self,
            DoctorCommand.self,
            StatusCommand.self,
            ConfigureCommand.self,
        ])
}
