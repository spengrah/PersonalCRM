package google

import (
	"net/mail"

	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"
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

// Contains reports whether one address a header yields is the user's.
// normalized is the address by matching.NormalizeEmail, as the parsers return
// it, and mailbox is the mailbox net/mail parsed from a well-formed header, or
// nil when the header did not parse (see parseSingleAddress and
// parseAddressList). A connected account's address counts by normalized either
// way, as Gmail sync has always compared it. An own address or an address at
// an own domain counts only for a non-nil mailbox: text recovered from a
// malformed header has no mailbox boundaries, so a fragment of it never counts
// by an own domain or own address (ING-051.unparsed-header-not-own).
func (m GmailMeSet) Contains(normalized string, mailbox *mail.Address) bool {
	if _, ok := m.accounts[normalized]; ok {
		return true
	}
	if mailbox == nil {
		return false
	}
	if _, ok := m.ownAddresses[sync.EmailOwnAddressKey(mailbox.Address)]; ok {
		return true
	}
	domain := domainOf(matching.NormalizeEmail(mailbox.Address))
	if domain == "" {
		return false
	}
	_, ok := m.domains[domain]
	return ok
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
