#!/usr/bin/env swift
// probe_cn_labels.swift — confirm the values in
// CNContactStoreReader.stableLabelMap still coincide with what
// `CNLabeledValue.localizedString(forLabel:)` returns under the
// host's current locale. Run when you suspect drift, or before
// adding a new entry to the map.
//
// Run:  swift mac-daemon/Scripts/probe_cn_labels.swift
//
// The script does NOT change anything — it just prints
// `constant  -> localized`. Compare to the map by eye.

import Contacts
import Foundation

let constants: [(String, String)] = [
    ("CNLabelHome",                    CNLabelHome),
    ("CNLabelWork",                    CNLabelWork),
    ("CNLabelSchool",                  CNLabelSchool),
    ("CNLabelOther",                   CNLabelOther),
    ("CNLabelEmailiCloud",             CNLabelEmailiCloud),
    ("CNLabelPhoneNumberiPhone",       CNLabelPhoneNumberiPhone),
    ("CNLabelPhoneNumberMobile",       CNLabelPhoneNumberMobile),
    ("CNLabelPhoneNumberMain",         CNLabelPhoneNumberMain),
    ("CNLabelPhoneNumberHomeFax",      CNLabelPhoneNumberHomeFax),
    ("CNLabelPhoneNumberWorkFax",      CNLabelPhoneNumberWorkFax),
    ("CNLabelPhoneNumberOtherFax",     CNLabelPhoneNumberOtherFax),
    ("CNLabelPhoneNumberPager",        CNLabelPhoneNumberPager),
    ("CNLabelPhoneNumberAppleWatch",   CNLabelPhoneNumberAppleWatch),
    ("CNLabelDateAnniversary",         CNLabelDateAnniversary),
    ("CNLabelURLAddressHomePage",      CNLabelURLAddressHomePage),
]

print("Locale: \(Locale.current.identifier)")
print("---")
for (name, value) in constants {
    let localized = CNLabeledValue<NSString>.localizedString(forLabel: value).lowercased()
    print("\(name.padding(toLength: 32, withPad: " ", startingAt: 0)) -> \(localized)")
}
