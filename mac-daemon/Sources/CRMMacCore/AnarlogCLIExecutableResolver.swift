import Foundation

/// The single place that resolves the Anarlog CLI: the configured `cli_path`
/// when set, otherwise `~/.local/bin/anarlog`, and nil unless it is executable.
/// Both the CLI client and the doctor use this resolver.
public enum AnarlogCLIExecutableResolver {

    public static func defaultCandidatePaths(homeDirectory: URL) -> [String] {
        [homeDirectory.appendingPathComponent(".local/bin/anarlog").path]
    }

    public static func resolve(cliPath: String?, homeDirectory: URL) -> String? {
        if let cliPath {
            return FileManager.default.isExecutableFile(atPath: cliPath) ? cliPath : nil
        }
        return defaultCandidatePaths(homeDirectory: homeDirectory)
            .first(where: FileManager.default.isExecutableFile(atPath:))
    }
}
