// `crm-mac doctor` runs the four health checks and exits with the
// number of FAIL entries.
import Foundation
import ArgumentParser
import CRMMacLifecycle

struct DoctorCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "doctor",
        abstract: "Run health checks: api-key file, agent registration, Pi reachability, config + state files.")

    mutating func run() async throws {
        let ctx = ProductionContext()
        let report = await ctx.doctor().run()
        for result in report.results {
            print("\(result.status.rawValue.padding(toLength: 5, withPad: " ", startingAt: 0)) \(result.name): \(result.details)")
        }
        if report.failCount > 0 {
            throw ExitCode(Int32(report.failCount))
        }
    }
}
