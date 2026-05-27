// AnarlogFSEventsWatcher — wraps FSEventStream so the anarlog_sessions
// plugin gets notified when ANY file under the configured sessions/
// directory changes. The watcher fires a single user-supplied closure
// per coalesced batch of FSEvents notifications; the closure typically
// triggers a `plugin.tick()` call.
//
// Lifecycle:
//   - init: stores config, NOT yet started
//   - start(): creates the FSEventStream, schedules on the
//     main run loop, starts streaming
//   - stop(): synchronously stops + invalidates + releases the stream
//
// CoreServices is linked in CRMMacSystem only (Package.swift) so the
// rest of the daemon stays Foundation-only.
//
// The closure receives no event details — the daemon's tick body
// re-walks the directory regardless, so distinguishing "which file"
// is unnecessary. This keeps the watcher's surface minimal.
//
// Error handling: the closure is invoked with no event details, and
// the wiring layer (DaemonCommand) wraps the closure body in
// do/catch + warning log so a thrown error from tick() doesn't
// silently disappear.
import Foundation
import CoreServices
import CRMMacCore

/// Watcher protocol so callers depend on a tiny abstraction (the
/// production impl is below; tests can ship a stub by conforming).
public protocol AnarlogFSEventsWatching: Sendable {
    func start() throws
    func stop()
}

public enum AnarlogFSEventsWatcherError: Error, Equatable, Sendable {
    case streamCreateFailed
    case alreadyStarted
}

public final class AnarlogFSEventsWatcher: AnarlogFSEventsWatching, @unchecked Sendable {
    private let path: String
    private let onChange: @Sendable () -> Void
    private let logger: LoggerProtocol
    /// Latency in seconds — FSEvents coalesces multiple changes within
    /// this window into a single callback. 1.5s matches the parent
    /// spec's "near real-time" intent for sessions (a recording
    /// finalizes its _summary.md a few hundred ms after _meta.json).
    private let coalescenceLatency: CFTimeInterval

    /// FSEventStreamRef under a Lock. Nil before start() / after
    /// stop(). The stream is created on start() and invalidated on
    /// stop(); both methods are idempotent and threadsafe via the
    /// streamLock.
    private var stream: FSEventStreamRef?
    private let streamLock = NSLock()

    public init(
        path: String,
        coalescenceLatency: CFTimeInterval = 1.5,
        logger: LoggerProtocol,
        onChange: @escaping @Sendable () -> Void
    ) {
        self.path = path
        self.coalescenceLatency = coalescenceLatency
        self.logger = logger
        self.onChange = onChange
    }

    deinit {
        // Best-effort cleanup if the caller forgot to stop().
        stop()
    }

    public func start() throws {
        streamLock.lock()
        defer { streamLock.unlock() }
        if stream != nil {
            throw AnarlogFSEventsWatcherError.alreadyStarted
        }

        // FSEvents C-callback retention: we pass `self` as the
        // info pointer via Unmanaged.passUnretained so the watcher's
        // lifetime governs the closure. Callers MUST keep the watcher
        // alive for as long as the stream is running.
        var context = FSEventStreamContext(
            version: 0,
            info: Unmanaged.passUnretained(self).toOpaque(),
            retain: nil,
            release: nil,
            copyDescription: nil)

        let pathArray = [path] as CFArray
        let flags: FSEventStreamCreateFlags = UInt32(
            kFSEventStreamCreateFlagFileEvents |
            kFSEventStreamCreateFlagNoDefer)

        guard let s = FSEventStreamCreate(
            kCFAllocatorDefault,
            { _, info, _, _, _, _ in
                // FSEventStreamCallback — recover the watcher via the
                // unmanaged info pointer + invoke the closure on the
                // run loop's dispatch thread.
                guard let info else { return }
                let watcher = Unmanaged<AnarlogFSEventsWatcher>
                    .fromOpaque(info).takeUnretainedValue()
                watcher.onChange()
            },
            &context,
            pathArray,
            FSEventStreamEventId(kFSEventStreamEventIdSinceNow),
            coalescenceLatency,
            flags)
        else {
            throw AnarlogFSEventsWatcherError.streamCreateFailed
        }

        // Schedule on a dedicated background dispatch queue rather than
        // the run loop. `FSEventStreamScheduleWithRunLoop` is
        // deprecated since macOS 13; `FSEventStreamSetDispatchQueue`
        // is the modern replacement and lets us avoid blocking the
        // main thread. We use a serial queue so callbacks arrive
        // in-order (FSEvents itself coalesces within the latency
        // window, so one queue per stream is sufficient).
        let queue = DispatchQueue(
            label: "xyz.spengrah.crm-mac.anarlog-fsevents",
            qos: .utility)
        FSEventStreamSetDispatchQueue(s, queue)
        if !FSEventStreamStart(s) {
            FSEventStreamInvalidate(s)
            FSEventStreamRelease(s)
            throw AnarlogFSEventsWatcherError.streamCreateFailed
        }
        stream = s
        logger.info("anarlog_sessions FSEvents: started", metadata: [
            "path": .private(path),
        ])
    }

    public func stop() {
        streamLock.lock()
        defer { streamLock.unlock() }
        guard let s = stream else { return }
        FSEventStreamStop(s)
        FSEventStreamInvalidate(s)
        FSEventStreamRelease(s)
        stream = nil
        logger.info("anarlog_sessions FSEvents: stopped")
    }
}
