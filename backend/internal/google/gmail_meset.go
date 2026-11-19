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

// Contains reports whether mailbox, one mailbox net/mail parsed from a header,
// is the user's. A header entry that does not parse as one mailbox has no
// address to compare, so its text never counts as the user: a nil mailbox is
// never the user's (RFC 5322 section 3.4).
func (m GmailMeSet) Contains(mailbox *mail.Address) bool {
	if mailbox == nil {
		return false
	}
	normAddr := matching.NormalizeEmail(mailbox.Address)
	if normAddr == "" {
		return false
	}
	if _, ok := m.accounts[normAddr]; ok {
		return true
	}
	if _, ok := m.ownAddresses[sync.EmailOwnAddressKey(mailbox.Address)]; ok {
		return true
	}
	domain := domainOf(normAddr)
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
