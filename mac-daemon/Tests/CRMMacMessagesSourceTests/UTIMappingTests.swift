import XCTest
@testable import CRMMacMessagesSource

final class UTIMappingTests: XCTestCase {
    // MARK: - bucket(forUTI:)

    func testImageBucketing() {
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.image"), .photo)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.image.heic"), .photo)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.png"), .photo)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.jpeg"), .photo)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.heic"), .photo)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "com.compuserve.gif"), .photo)
    }

    func testAudioBucketing() {
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.audio"), .audio)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.mp3"), .audio)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "com.apple.coreaudio-format"), .audio)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "com.apple.m4a-audio"), .audio)
    }

    func testVideoBucketing() {
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.movie"), .video)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.mpeg-4"), .video)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "com.apple.quicktime-movie"), .video)
    }

    func testDocumentBucketing() {
        XCTAssertEqual(UTIMapping.bucket(forUTI: "com.adobe.pdf"), .document)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.pdf"), .document)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "com.microsoft.word.doc"), .document)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.plain-text"), .document)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "public.composite-content"), .document)
    }

    func testUnknownUTIIsOther() {
        XCTAssertEqual(UTIMapping.bucket(forUTI: "com.example.unknown"), .other)
        XCTAssertEqual(UTIMapping.bucket(forUTI: ""), .other)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "garbage"), .other)
    }

    func testCaseInsensitive() {
        XCTAssertEqual(UTIMapping.bucket(forUTI: "PUBLIC.IMAGE"), .photo)
        XCTAssertEqual(UTIMapping.bucket(forUTI: "Com.Adobe.PDF"), .document)
    }

    // MARK: - bucket(forAttachmentUTIs:) — first-non-other wins

    func testFirstNonOtherWins() {
        // Image first, ignored other.
        XCTAssertEqual(
            UTIMapping.bucket(forAttachmentUTIs: ["public.image", "com.example.unknown"]),
            .photo)
        // Other first, then video — first non-other wins.
        XCTAssertEqual(
            UTIMapping.bucket(forAttachmentUTIs: ["com.example.unknown", "public.movie"]),
            .video)
    }

    func testAllOtherStaysOther() {
        XCTAssertEqual(
            UTIMapping.bucket(forAttachmentUTIs: ["x", "y", "z"]),
            .other)
    }

    func testEmptyAttachmentList() {
        XCTAssertEqual(UTIMapping.bucket(forAttachmentUTIs: []), .other)
    }

    // MARK: - resolve(attachmentUTIs:hasText:)

    func testResolveNoAttachmentWithTextIsText() {
        XCTAssertEqual(UTIMapping.resolve(attachmentUTIs: [], hasText: true), .text)
    }

    func testResolveNoAttachmentNoTextIsOther() {
        XCTAssertEqual(UTIMapping.resolve(attachmentUTIs: [], hasText: false), .other)
    }

    func testResolveAttachmentTakesPrecedenceOverText() {
        XCTAssertEqual(
            UTIMapping.resolve(attachmentUTIs: ["public.image"], hasText: true),
            .photo)
    }

    func testResolveAttachmentNoText() {
        XCTAssertEqual(
            UTIMapping.resolve(attachmentUTIs: ["public.movie"], hasText: false),
            .video)
    }

    func testResolveAllOtherAttachmentsIsOtherRegardlessOfText() {
        // Spec resolution: if all attachments bucket to .other and the
        // message has text, the attachment still wins (the bucket
        // represents the message kind, not "what to display"). This is
        // a v1 simplification; the message is .other not .text.
        XCTAssertEqual(
            UTIMapping.resolve(attachmentUTIs: ["x", "y"], hasText: true),
            .other)
    }
}
