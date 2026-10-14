// rescan-messages — one-time operator script (`swift run rescan-messages`).
// See RescanMessages.swift for what it does.
import Foundation
import CRMMacPiClient

exit(await rescanMessages(RescanEnvironment(
    home: URL(fileURLWithPath: NSHomeDirectory()),
    makePiClient: { PiClient(baseURL: $0) },
    stdout: { print($0) },
    stderr: { FileHandle.standardError.write(Data(($0 + "\n").utf8)) })))
