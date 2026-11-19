package google

import (
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"

	"google.golang.org/api/gmail/v1"
)

// GmailMeSet answers "is this address the user" for Gmail sync, the Gmail
// identifier rematch scan and Gmail participant discovery (ING-051, IMP-051).
// An address is the user's when it is a connected account's address, an
// address at an own domain, or an own address. buildGmailMeSet is its one
// constructor and GmailSyncProvider.MeSet the one way Gmail code obtains it;
// the zero value holds no address.
type GmailMeSet struct {
	// accounts holds the connected accounts' addresses, normalized by
	// matching.NormalizeEmail, the form a header mailbox has always been
	// compared to them in.
	accounts map[string]struct{}
	// ownAddresses holds the own addresses by sync.EmailOwnAddressKey, so
	// only the configured mailbox, every character of its local part
	// included, is the user.
	ownAddresses map[string]struct{}
	domains      map[string]struct{}
}

// Contains reports whether one address a header yields is the user's. A
// connected account's address counts by the address's normalized form whether
// or not its header parsed, as Gmail sync has always compared it. An own
// address or an address at an own domain counts only for an address with a
// mailbox, one whose header parsed cleanly (see parseSingleAddress and
// parseAddressList): text recovered from a malformed header has no mailbox
// boundaries, so a fragment of it never counts by an own domain or own address
// (ING-051.unparsed-header-not-own).
func (m GmailMeSet) Contains(a parsedAddress) bool {
	if _, ok := m.accounts[a.normalized]; ok {
		return true
	}
	if a.mailbox == nil {
		return false
	}
	if _, ok := m.ownAddresses[sync.EmailOwnAddressKey(a.mailbox.Address)]; ok {
		return true
	}
	domain := domainOf(matching.NormalizeEmail(a.mailbox.Address))
	if domain == "" {
		return false
	}
	_, ok := m.domains[domain]
	return ok
}

// SentByUser reports whether Gmail sync counts msg as mail the user sent:
// whether its From address is the user's (Contains). Gmail sync stores mail
// the user sent as outbound mail for each contact among its recipients, and
// synthetic batch replay takes a message's direction from this same test.
func (m GmailMeSet) SentByUser(msg *gmail.Message) bool {
	return m.Contains(headerAddress(msg, "From"))
}

// buildGmailMeSet builds the me-set from the connected accounts' addresses and
// the own domains and own addresses configured on every given email sync
// state. The union over states makes configuration on one account count when
// another account's mailbox is synced.
func buildGmailMeSet(accountAddrs []string, states []repository.SyncState) GmailMeSet {
	m := GmailMeSet{
		accounts:     make(map[string]struct{}, len(accountAddrs)),
		ownAddresses: make(map[string]struct{}),
		domains:      make(map[string]struct{}),
	}
	for _, a := range accountAddrs {
		if n := matching.NormalizeEmail(a); n != "" {
			m.accounts[n] = struct{}{}
		}
	}
	for _, st := range states {
		for d := range sync.EmailOwnDomains.Read(st.Metadata) {
			m.domains[d] = struct{}{}
		}
		for a := range sync.EmailOwnAddresses.Read(st.Metadata) {
			m.ownAddresses[a] = struct{}{}
		}
	}
	return m
}
