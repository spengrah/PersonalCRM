// ContentHasherTests mirror the Go-side TestComputeContentHash_*
// suite at backend/internal/service/external_contact_hash_test.go.
// Both sides exercise the same recipe; drift between them is the
// single most catastrophic failure mode of the icloud_contacts
// source plugin (mismatched hashes cause the Pi to reject every
// upserted event), so the parity is duplicated here in addition to
// the cross-language fixture battery.
import XCTest
@testable import CRMMacCore

final class ContentHasherTests: XCTestCase {

    func testStableAcrossKeyOrder() throws {
        let a = #"{"b":1,"a":2}"#
        let b = #"{"a":2,"b":1}"#
        let hashA = try ContentHasher.contentHash(for: Data(a.utf8))
        let hashB = try ContentHasher.contentHash(for: Data(b.utf8))
        XCTAssertEqual(hashA, hashB)
    }

    func testRemovesHostID() throws {
        let withHost = #"{"entity_id":"e1","host_id":"h-uuid","display_name":"x"}"#
        let withoutHost = #"{"entity_id":"e1","display_name":"x"}"#
        let hashWith = try ContentHasher.contentHash(for: Data(withHost.utf8))
        let hashWithout = try ContentHasher.contentHash(for: Data(withoutHost.utf8))
        XCTAssertEqual(hashWith, hashWithout,
                       "host_id must be stripped before hashing")
    }

    func testNFCAndNFDProduceDistinctHashes() throws {
        let nfc = "{\"name\":\"\\u00e9\"}"
        let nfd = "{\"name\":\"e\\u0301\"}"
        let h1 = try ContentHasher.contentHash(for: Data(nfc.utf8))
        let h2 = try ContentHasher.contentHash(for: Data(nfd.utf8))
        XCTAssertNotEqual(h1, h2)
    }

    func testRejectsInvalidJSON() {
        XCTAssertThrowsError(
            try ContentHasher.contentHash(for: Data(#"{"missing-close":"#.utf8)))
        XCTAssertThrowsError(
            try ContentHasher.contentHash(for: Data("not json at all".utf8)))
    }

    func testEmptyObject() throws {
        let h = try ContentHasher.contentHash(for: Data("{}".utf8))
        XCTAssertEqual(h.count, 64)
        for ch in h {
            XCTAssertTrue("0123456789abcdef".contains(ch),
                          "hash must be lowercase hex")
        }
    }

    func testNonAsciiPreserved() throws {
        let input = #"{"name":"José 👋","city":"São Paulo"}"#
        let h1 = try ContentHasher.contentHash(for: Data(input.utf8))
        let h2 = try ContentHasher.contentHash(for: Data(input.utf8))
        XCTAssertEqual(h1, h2)
        XCTAssertEqual(h1.count, 64)
    }

    /// JSONSerialization silently collapses duplicate JSON keys, so
    /// a payload like `{"a":1,"a":2}` parses into a single-keyed
    /// dictionary BEFORE the canonicalizer runs. The hasher
    /// therefore produces a value rather than throwing — this test
    /// documents the asymmetry with gowebpki/jcs (which rejects
    /// duplicate keys at parse time). The daemon's only call site
    /// is `JSONEncoder.encode(Encodable)`, which never produces
    /// duplicate keys; this asymmetry is unreachable in production.
    ///
    /// Which duplicate value Foundation keeps (first vs last) is
    /// implementation-defined and has varied across OS versions, so
    /// we do NOT lock the test to a specific winner. Instead we
    /// observe what Foundation kept and assert the hasher is
    /// consistent with that observation.
    func testDuplicateKeyParseSilentlyDedupsViaFoundation() throws {
        let dup = #"{"a":1,"a":2}"#
        let dupData = Data(dup.utf8)
        guard let parsed = try JSONSerialization.jsonObject(with: dupData) as? [String: Any],
              let kept = parsed["a"] as? NSNumber else {
            XCTFail("Foundation did not parse duplicate-key payload as expected")
            return
        }
        let canonicalEquivalent = #"{"a":\#(kept.intValue)}"#
        let h1 = try ContentHasher.contentHash(for: dupData)
        let h2 = try ContentHasher.contentHash(for: Data(canonicalEquivalent.utf8))
        XCTAssertEqual(h1, h2,
                       "hash of duplicate-key payload must match the single-keyed form Foundation collapsed it to")
    }
}
