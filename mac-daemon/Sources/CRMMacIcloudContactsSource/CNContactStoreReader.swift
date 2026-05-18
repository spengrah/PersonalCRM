// CNContactStoreReader is the thin shell over Apple's Contacts
// framework that the icloud_contacts source plugin uses to (a) list
// CNContainers, (b) full-fetch all contacts in an allowlisted set of
// containers, (c) walk the change-history token forward, and (d)
// capture the current token BEFORE a full scan so anything edited
// during the scan window is naturally caught by the next delta tick.
//
// Production: imports Contacts framework. Tests inject a protocol
// stub (`ContactStoreReader` is the abstraction) so the plugin's
// per-tick logic is fully unit-testable without permission.
//
// `import Contacts` is intentionally isolated to this target so the
// rest of the daemon stays Foundation-only.
import Foundation
@preconcurrency import Contacts
import CRMMacCore

/// Result of a single change-history walk. The plugin uses
/// `newToken` as the cursor on the next tick.
public struct ChangeHistoryResult: Sendable, Equatable {
    public let events: [ContactChange]
    public let newToken: Data

    public init(events: [ContactChange], newToken: Data) {
        self.events = events
        self.newToken = newToken
    }
}

/// Errors surfaced by the reader. Recoverable cases (`tokenInvalid`)
/// route the plugin to the recovery path; everything else marks the
/// source unhealthy.
public enum CNContactStoreReaderError: Error, Equatable, CustomStringConvertible {
    case tokenInvalid(underlying: String)
    case underlying(String)

    public var description: String {
        switch self {
        case .tokenInvalid(let s):
            return "CNContactStoreReader: change-history token invalid (\(s))"
        case .underlying(let s):
            return "CNContactStoreReader: \(s)"
        }
    }
}

/// Protocol the plugin depends on; the production type
/// `CNContactStoreReader` implements it, tests inject a stub.
public protocol ContactStoreReader: Sendable {
    func listContainers() throws -> [ContainerInfo]
    func fullFetch(containerIdentifiers: [String]) throws -> [ContactRecord]
    func changeHistory(from token: Data?) throws -> ChangeHistoryResult
    func currentToken() throws -> Data
}

/// Production reader. `@unchecked Sendable` because `CNContactStore`
/// is documented thread-safe for the methods we call but Foundation
/// lacks formal Sendable.
public final class CNContactStoreReader: ContactStoreReader, @unchecked Sendable {
    private let store: CNContactStore

    public init(store: CNContactStore = CNContactStore()) {
        self.store = store
    }

    public func listContainers() throws -> [ContainerInfo] {
        let containers: [CNContainer]
        do {
            containers = try store.containers(matching: nil)
        } catch {
            throw CNContactStoreReaderError.underlying(String(describing: error))
        }
        return containers.map { c in
            let kind = Self.mapKind(c.type)
            return ContainerInfo(
                identifier: c.identifier,
                name: c.name,
                type: kind,
                defaultIncluded: Self.isDefaultIncluded(kind: kind, name: c.name))
        }
    }

    public func fullFetch(containerIdentifiers: [String]) throws -> [ContactRecord] {
        var out: [ContactRecord] = []
        let keysToFetch = Self.contactKeysToFetch()
        for cid in containerIdentifiers {
            let predicate = CNContact.predicateForContactsInContainer(withIdentifier: cid)
            do {
                let cnContacts = try store.unifiedContacts(
                    matching: predicate,
                    keysToFetch: keysToFetch)
                for cn in cnContacts {
                    out.append(Self.mapRecord(cn, containerIdentifier: cid))
                }
            } catch {
                throw CNContactStoreReaderError.underlying(
                    "fullFetch container=\(cid): \(error)")
            }
        }
        return out
    }

    public func changeHistory(from token: Data?) throws -> ChangeHistoryResult {
        let request = CNChangeHistoryFetchRequest()
        request.startingToken = token
        // We need the contact body for adds/updates so the plugin
        // can shape + hash without a follow-up fetch.
        request.shouldUnifyResults = true
        request.additionalContactKeyDescriptors = Self.contactKeysToFetch()

        let fetch: AnyObject
        do {
            // `enumeratorForChangeHistoryFetchRequest:error:` is
            // NS_SWIFT_UNAVAILABLE in the Contacts headers, so we
            // invoke it via the Objective-C runtime. The selector
            // returns nil on error (NSError out-param set); we map
            // CNError.changeHistoryInvalidAnchor to the typed
            // .tokenInvalid case so the plugin can route to the
            // recovery path.
            fetch = try Self.invokeEnumeratorForChangeHistory(store: store, request: request)
        } catch let err as NSError where err.domain == CNErrorDomain
                && err.code == CNError.Code.changeHistoryInvalidAnchor.rawValue {
            throw CNContactStoreReaderError.tokenInvalid(
                underlying: "CNError.changeHistoryInvalidAnchor")
        } catch let e as CNContactStoreReaderError {
            throw e
        } catch {
            throw CNContactStoreReaderError.underlying(String(describing: error))
        }

        guard let enumerator = (fetch.value(forKey: "value") as? NSEnumerator) else {
            throw CNContactStoreReaderError.underlying(
                "CNFetchResult.value is not an NSEnumerator")
        }
        guard let tokenObj = fetch.value(forKey: "currentHistoryToken") as? Data else {
            throw CNContactStoreReaderError.underlying(
                "missing currentHistoryToken in CNFetchResult")
        }

        var events: [ContactChange] = []
        while let raw = enumerator.nextObject() {
            if let add = raw as? CNChangeHistoryAddContactEvent {
                events.append(.add(Self.mapRecord(
                    add.contact,
                    containerIdentifier: add.containerIdentifier ?? "")))
            } else if let upd = raw as? CNChangeHistoryUpdateContactEvent {
                // CNChangeHistoryUpdateContactEvent does NOT carry a
                // containerIdentifier — Apple's header only exposes
                // it on AddContactEvent. Resolve via a follow-up
                // CNContainer fetch. Three outcomes:
                //   - hit: forward the update with the resolved id.
                //   - miss (contact deleted between events): emit a
                //     synthetic delete so the Pi tombstones it; the
                //     subsequent .delete event (if any) dedups at the
                //     Pi's event log.
                //   - throw: fail-closed with .unknown so the plugin
                //     holds the cursor + sets the recovery flag.
                //     Silently emitting with cid="" would drop the
                //     update under the plugin's allowlist filter and
                //     still advance the cursor — silent data loss.
                let resolved: String?
                do {
                    resolved = try resolveContainerID(
                        forContactIdentifier: upd.contact.identifier)
                } catch {
                    events.append(.unknown(rawEventDescription:
                        "resolveContainerID threw for update: \(error)"))
                    continue
                }
                if let cid = resolved {
                    events.append(.update(Self.mapRecord(
                        upd.contact, containerIdentifier: cid)))
                } else {
                    // Contact has no container — it was deleted
                    // between the change-history walk and the
                    // resolve call. Emit as a delete so the Pi
                    // tombstones the row.
                    events.append(.delete(identifier: upd.contact.identifier))
                }
            } else if let del = raw as? CNChangeHistoryDeleteContactEvent {
                events.append(.delete(identifier: del.contactIdentifier))
            } else if let event = raw as? CNChangeHistoryEvent {
                // Unrecognized subtype — DO NOT silently drop. The
                // plugin's dispatcher matches on `.unknown` and goes
                // fail-closed: log + mark unhealthy + set recovery
                // flag + abort cursor advance.
                events.append(.unknown(rawEventDescription: String(describing: event)))
            }
        }
        return ChangeHistoryResult(events: events, newToken: tokenObj)
    }

    /// Look up the CNContainer identifier for a given contact.
    /// Returns nil ONLY when the contact has no matching container
    /// (deleted upstream between the change-history walk and this
    /// lookup). Throws on any other framework error so the plugin
    /// can fail-closed via the `.unknown` event path; previously
    /// this swallowed errors as nil and the plugin's allowlist
    /// filter then dropped the update silently while advancing the
    /// cursor — a silent data-loss path the caller could never
    /// detect.
    private func resolveContainerID(forContactIdentifier id: String) throws -> String? {
        let predicate = CNContainer.predicateForContainerOfContact(withIdentifier: id)
        let containers: [CNContainer]
        do {
            containers = try store.containers(matching: predicate)
        } catch {
            throw CNContactStoreReaderError.underlying(
                "containers(matching:predicateForContact id=\(id)): \(error)")
        }
        return containers.first?.identifier
    }

    public func currentToken() throws -> Data {
        // `CNContactStore.currentHistoryToken` is a Swift-available
        // property since macOS 10.15. The token may be nil before
        // the store has observed any changes — surface that as a
        // typed error so the plugin can route to recovery.
        guard let token = store.currentHistoryToken else {
            throw CNContactStoreReaderError.underlying(
                "currentToken: CNContactStore.currentHistoryToken is nil")
        }
        return token
    }

    /// ObjC-runtime invocation of
    /// `-[CNContactStore enumeratorForChangeHistoryFetchRequest:error:]`,
    /// which is `NS_SWIFT_UNAVAILABLE` in the Contacts headers.
    /// Returns the `CNFetchResult` opaque object; callers extract
    /// `.value` (NSEnumerator) and `.currentHistoryToken` (NSData)
    /// via KVC. Bridges the NSError out-param into a Swift throw.
    private static func invokeEnumeratorForChangeHistory(
        store: CNContactStore,
        request: CNChangeHistoryFetchRequest
    ) throws -> AnyObject {
        let selector = NSSelectorFromString("enumeratorForChangeHistoryFetchRequest:error:")
        guard store.responds(to: selector) else {
            throw CNContactStoreReaderError.underlying(
                "CNContactStore does not respond to enumeratorForChangeHistoryFetchRequest:error:")
        }
        // Build the IMP signature: takes (self, _cmd, request, NSErrorPointer)
        // and returns id. Use NSObject.method(for:) + unsafeBitCast.
        typealias FuncSig = @convention(c) (AnyObject, Selector, AnyObject, AutoreleasingUnsafeMutablePointer<NSError?>?) -> AnyObject?
        guard let methodImpl = store.method(for: selector) else {
            throw CNContactStoreReaderError.underlying(
                "method-for-selector returned nil for enumeratorForChangeHistoryFetchRequest")
        }
        let typed = unsafeBitCast(methodImpl, to: FuncSig.self)
        var nsErr: NSError? = nil
        let result = typed(store, selector, request, &nsErr)
        if let err = nsErr {
            throw err
        }
        guard let unwrapped = result else {
            throw CNContactStoreReaderError.underlying(
                "enumeratorForChangeHistoryFetchRequest returned nil with no error")
        }
        return unwrapped
    }

    // MARK: - mapping helpers

    static func mapKind(_ raw: CNContainerType) -> ContainerKind {
        switch raw {
        case .local:      return .local
        case .cardDAV:    return .cardDAV
        case .exchange:   return .exchange
        case .unassigned: return .unassigned
        @unknown default:
            return .unknown(rawValue: raw.rawValue)
        }
    }

    /// Mirrors `ContainerPicker.defaults(for:)`. Kept inline here so
    /// the production adapter can pre-populate `defaultIncluded`
    /// without depending on the picker module.
    static func isDefaultIncluded(kind: ContainerKind, name: String) -> Bool {
        switch kind {
        case .local:
            return true
        case .cardDAV:
            return name.lowercased() == "icloud"
        case .exchange, .unassigned, .unknown:
            return false
        }
    }

    /// Keys needed to populate every field in `ContactRecord`. Keep
    /// in sync with `mapRecord` below.
    static func contactKeysToFetch() -> [CNKeyDescriptor] {
        [
            CNContactIdentifierKey as CNKeyDescriptor,
            CNContactGivenNameKey as CNKeyDescriptor,
            CNContactFamilyNameKey as CNKeyDescriptor,
            CNContactOrganizationNameKey as CNKeyDescriptor,
            CNContactJobTitleKey as CNKeyDescriptor,
            CNContactBirthdayKey as CNKeyDescriptor,
            CNContactEmailAddressesKey as CNKeyDescriptor,
            CNContactPhoneNumbersKey as CNKeyDescriptor,
            CNContactPostalAddressesKey as CNKeyDescriptor,
            CNContactFormatter.descriptorForRequiredKeys(for: .fullName),
        ]
    }

    static func mapRecord(_ cn: CNContact, containerIdentifier: String) -> ContactRecord {
        let display = CNContactFormatter.string(from: cn, style: .fullName)
        let firstName = cn.givenName.isEmpty ? nil : cn.givenName
        let lastName = cn.familyName.isEmpty ? nil : cn.familyName
        let organization = cn.organizationName.isEmpty ? nil : cn.organizationName
        let jobTitle = cn.jobTitle.isEmpty ? nil : cn.jobTitle
        let birthday = cn.birthday

        let emails = cn.emailAddresses.enumerated().map { i, labeled -> ContactEmail in
            ContactEmail(
                value: labeled.value as String,
                type: Self.localizedLabel(labeled.label),
                primary: i == 0)
        }
        let phones = cn.phoneNumbers.enumerated().map { i, labeled -> ContactPhone in
            ContactPhone(
                value: labeled.value.stringValue,
                type: Self.localizedLabel(labeled.label),
                primary: i == 0)
        }
        let addresses = cn.postalAddresses.map { labeled -> ContactAddress in
            let formatted = CNPostalAddressFormatter.string(
                from: labeled.value, style: .mailingAddress)
            return ContactAddress(
                formatted: formatted,
                type: Self.localizedLabel(labeled.label))
        }

        return ContactRecord(
            identifier: cn.identifier,
            containerIdentifier: containerIdentifier,
            displayName: display,
            firstName: firstName,
            lastName: lastName,
            emails: emails,
            phones: phones,
            addresses: addresses,
            organization: organization,
            jobTitle: jobTitle,
            birthday: birthday)
    }

    /// CNLabeledValue label normalization — map Apple's well-known
    /// `CNLabel*` constants to stable, lowercase, locale-independent
    /// strings. Custom user labels (e.g. "Mom") pass through with the
    /// wrapper stripped if present, then lowercased.
    ///
    /// The previous implementation used
    /// `CNLabeledValue.localizedString(forLabel:)`, which is
    /// locale-sensitive: the returned string depends on the host's
    /// macOS language. Because the label is part of the canonicalized
    /// payload that feeds SHA-256(JCS(...)), a locale change would
    /// produce a different hash for an unchanged contact, triggering a
    /// spurious re-sync and breaking `/known-ids` parity. The map
    /// keeps the wire shape stable across locales.
    ///
    /// Migration: rows already on the Pi were hashed under the old
    /// path. Contacts whose labels are all in `stableLabelMap` and
    /// running under `en_US` see no drift. Any wrapped label NOT in
    /// the map (a future Apple `CNLabel*` constant we haven't
    /// enumerated, or a third-party-written wrapper string) will
    /// re-hash on first sync after this change because the fallback
    /// strips the wrapper while the old path emitted it raw. One-time
    /// write amplification, not data loss.
    static func localizedLabel(_ raw: String?) -> String? {
        guard let raw, !raw.isEmpty else { return nil }
        if let mapped = stableLabelMap[raw] {
            return mapped
        }
        // Unknown label. Strip Apple's `_$!<…>!$_` wrapper if present
        // (covers any CNLabel* constant we haven't enumerated), then
        // lowercase. Custom user labels lack the wrapper and pass
        // through with only the lowercase step.
        let unwrapped = stripCNLabelWrapper(raw)
        if unwrapped.isEmpty { return nil }
        return unwrapped.lowercased()
    }

    /// Stable, locale-independent mapping for Apple's well-known CN
    /// label constants. The string values are the design contract —
    /// the chosen shorthands happen to coincide with what
    /// `CNLabeledValue.localizedString(forLabel:)` returns under
    /// `en_US` (so `en_US` users see no migration hash drift), but the
    /// invariant we promise is "stable across locales", NOT "tracks
    /// Apple's localized output." Verify with
    /// `mac-daemon/Scripts/probe_cn_labels.swift` if you need to
    /// reconfirm against a current SDK.
    private static let stableLabelMap: [String: String] = [
        // --- Currently-exercised by ContactKeysToFetch (email, phone,
        // postal address). ---
        // Generic (used by emails, phones, addresses)
        CNLabelHome: "home",
        CNLabelWork: "work",
        CNLabelSchool: "school",
        CNLabelOther: "other",
        // Email-specific
        CNLabelEmailiCloud: "icloud",
        // Phone-specific
        CNLabelPhoneNumberiPhone: "iphone",
        CNLabelPhoneNumberMobile: "mobile",
        CNLabelPhoneNumberMain: "main",
        CNLabelPhoneNumberHomeFax: "home fax",
        CNLabelPhoneNumberWorkFax: "work fax",
        CNLabelPhoneNumberOtherFax: "other fax",
        CNLabelPhoneNumberPager: "pager",
        CNLabelPhoneNumberAppleWatch: "apple watch",
        // --- Forward-compat: not exercised today (contactKeysToFetch
        // doesn't request URL or date-with-label fields), included so a
        // future field addition doesn't silently fall to the fallback
        // path. ---
        CNLabelDateAnniversary: "anniversary",
        CNLabelURLAddressHomePage: "homepage",
    ]

    /// Strip Apple's `_$!<X>!$_` wrapper if present. Unknown wrapped
    /// values get a deterministic shorthand instead of leaking the
    /// raw wrapper into the wire payload.
    private static func stripCNLabelWrapper(_ s: String) -> String {
        let prefix = "_$!<"
        let suffix = ">!$_"
        guard s.hasPrefix(prefix), s.hasSuffix(suffix) else { return s }
        let inner = s.dropFirst(prefix.count).dropLast(suffix.count)
        return String(inner)
    }
}
