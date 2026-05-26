import XCTest
@testable import CRMMacPhoneCallsSource

final class ServiceDerivationTests: XCTestCase {
    func testTelephonyAnyCallTypeMapsToVoice() {
        XCTAssertEqual(
            ServiceDerivation.resolve(provider: "com.apple.Telephony", callType: 0),
            .voice)
        XCTAssertEqual(
            ServiceDerivation.resolve(provider: "com.apple.Telephony", callType: 8),
            .voice)
        XCTAssertEqual(
            ServiceDerivation.resolve(provider: "com.apple.Telephony", callType: nil),
            .voice)
    }

    func testFaceTimeCallType8IsAudio() {
        XCTAssertEqual(
            ServiceDerivation.resolve(provider: "com.apple.FaceTime", callType: 8),
            .facetimeAudio)
    }

    func testFaceTimeCallType16IsVideo() {
        XCTAssertEqual(
            ServiceDerivation.resolve(provider: "com.apple.FaceTime", callType: 16),
            .facetimeVideo)
    }

    func testFaceTimeUnknownCallTypeIsNil() {
        XCTAssertNil(ServiceDerivation.resolve(provider: "com.apple.FaceTime", callType: 0))
        XCTAssertNil(ServiceDerivation.resolve(provider: "com.apple.FaceTime", callType: 99))
        XCTAssertNil(ServiceDerivation.resolve(provider: "com.apple.FaceTime", callType: nil))
    }

    func testUnknownProviderIsNil() {
        XCTAssertNil(ServiceDerivation.resolve(provider: "com.apple.Unknown", callType: 8))
        XCTAssertNil(ServiceDerivation.resolve(provider: "telegram", callType: nil))
    }

    func testEmptyOrNilProviderIsNil() {
        XCTAssertNil(ServiceDerivation.resolve(provider: nil, callType: 8))
        XCTAssertNil(ServiceDerivation.resolve(provider: "", callType: 8))
    }

    func testProviderMatchIsCaseInsensitive() {
        // Apple has been observed to vary capitalization across macOS
        // releases on com.apple.* reverse-DNS strings.
        XCTAssertEqual(
            ServiceDerivation.resolve(provider: "COM.APPLE.TELEPHONY", callType: nil),
            .voice)
        XCTAssertEqual(
            ServiceDerivation.resolve(provider: "com.apple.facetime", callType: 16),
            .facetimeVideo)
    }

    func testCanonicalServiceRawValuesMatchPiCheckConstraint() {
        // These values are matched verbatim against the
        // phone_call.service CHECK constraint on the Pi side.
        XCTAssertEqual(PhoneCallService.voice.rawValue, "voice")
        XCTAssertEqual(PhoneCallService.facetimeAudio.rawValue, "facetime_audio")
        XCTAssertEqual(PhoneCallService.facetimeVideo.rawValue, "facetime_video")
    }
}
