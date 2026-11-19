// Integration coverage for the Gmail me-set built from configuration (ING-051,
// IMP-051): mail from or to an address at a configured own domain, or a
// configured own address, is the user's for direction, as mail at a connected
// account's own address is, and discovery treats those addresses as the user.
// Each test drives the REAL GmailSyncProvider through the synthetic replay
// harness (its database clone, contact seeding, bus and fake Gmail fetcher)
// and reads the configuration from enabled email sync states persisted in the
// clone, the way production reads it; the harness's real email consumer
// derives the interactions. Domains and addresses are placeholders (repository
// PII rule).
package tests

import (
	"context"
	"net/mail"
	"strings"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	syncpkg "personal-crm/backend/internal/sync"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ownAddressEnv is one replay harness plus the email sync states and messages
// a test configures on it.
type ownAddressEnv struct {
	ctx      context.Context
	h        *synthetic.Harness
	gen      *factory.Generator
	prefix   string
	syncRepo *repository.SyncRepository
}

func newOwnAddressEnv(t *testing.T) *ownAddressEnv {
	t.Helper()
	ctx := t.Context()
	h := synthetic.NewHarnessForNamespace(t, ctx, "own"+uuid.NewString()[:8], factory.DefaultSeed)
	return &ownAddressEnv{
		ctx:      ctx,
		h:        h,
		gen:      h.Generator(),
		prefix:   h.Generator().Prefix(),
		syncRepo: repository.NewSyncRepository(h.Database().Queries),
	}
}

// emailAccount persists an enabled email sync state for account, with a
// backfill floor a few days behind the generator's messages so one sweep
// reaches them, and sets its own-domain and own-address configuration through
// the lists' one writer, as crm-admin does.
func (e *ownAddressEnv) emailAccount(t *testing.T, account string, ownDomains, ownAddresses []string) *repository.SyncState {
	t.Helper()
	state, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
		Source:    google.GmailSourceName,
		AccountID: &account,
		Enabled:   true,
		Metadata:  map[string]any{"backfill_since": e.gen.Anchor().AddDate(0, 0, -3).UTC().Format("2006-01-02")},
	})
	require.NoError(t, err)
	for _, c := range []struct {
		list    syncpkg.EmailOwnList
		entries []string
	}{{syncpkg.EmailOwnDomains, ownDomains}, {syncpkg.EmailOwnAddresses, ownAddresses}} {
		if len(c.entries) > 0 {
			_, err := c.list.Set(e.ctx, e.syncRepo, state.ID, strings.Join(c.entries, ","))
			require.NoError(t, err)
		}
	}
	return state
}

// contact seeds a contact with one email method at email, or at a generated
// address when email is empty, and returns it with that address.
func (e *ownAddressEnv) contact(t *testing.T, email string) (*repository.Contact, string) {
	t.Helper()
	spec := e.gen.Contact(factory.WithEmail())
	if email != "" {
		spec.Methods = []factory.MethodSpec{{Type: "email", Value: email, IsPrimary: true}}
		spec.Email = email
	}
	contact, err := e.h.SeedContact(e.ctx, spec)
	require.NoError(t, err)
	return contact, spec.Email
}

// message builds a message in account's mailbox, in a thread of its own,
// carrying exactly the given address headers.
func (e *ownAddressEnv) message(account string, addrs factory.GmailAddresses) factory.GmailMessageSpec {
	return e.gen.GmailMessageWithShape(factory.ContactSpec{}, factory.MatchSeeded,
		factory.GmailMessageShape{Mailbox: account, Addresses: &addrs})
}

// bare is an address with no display name.
func bare(email string) factory.GmailAddress { return factory.GmailAddress{Email: email} }

// bares are addresses with no display name.
func bares(emails ...string) []factory.GmailAddress {
	out := make([]factory.GmailAddress, len(emails))
	for i, email := range emails {
		out[i] = bare(email)
	}
	return out
}

// provider builds the harness's Gmail provider over msgs, all in one account's
// mailbox.
func (e *ownAddressEnv) provider(t *testing.T, msgs ...factory.GmailMessageSpec) *google.GmailSyncProvider {
	t.Helper()
	p, err := e.h.GmailProvider(msgs...)
	require.NoError(t, err)
	return p
}

// sync runs one Gmail sync of state's mailbox, then drains the jobs it
// enqueued.
func (e *ownAddressEnv) sync(t *testing.T, p *google.GmailSyncProvider, state *repository.SyncState) {
	t.Helper()
	_, err := p.Sync(e.ctx, state, nil)
	require.NoError(t, err)
	require.NoError(t, e.h.DrainGateB(e.ctx))
}

// requireStored asserts contactID holds exactly one stored email row, with
// direction, for msg, and that the consumer recorded an email interaction in
// that direction from it.
func (e *ownAddressEnv) requireStored(t *testing.T, contactID uuid.UUID, msg factory.GmailMessageSpec, direction string) {
	t.Helper()
	rows, err := e.h.CommsRepo().ListByContact(e.ctx, contactID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "one stored email row for the contact")
	require.Equal(t, msg.ExternalID, rows[0].ExternalID)
	require.Equal(t, direction, rows[0].Direction)

	waitForCommsProcessedByExternalID(t, e.ctx, e.h.CommsRepo(), msg.ExternalID, contactID)
	interactions := listEmailInteractionsFor(t, e.ctx, e.h.InteractionRepo(), contactID)
	require.Len(t, interactions, 1, "one email interaction for the contact")
	require.Equal(t, direction, interactions[0].Direction)
}

// requireNothingStored asserts contactID holds no stored email row.
func (e *ownAddressEnv) requireNothingStored(t *testing.T, contactID uuid.UUID) {
	t.Helper()
	rows, err := e.h.CommsRepo().ListByContact(e.ctx, contactID)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestGmailOwnDomain_SyncStoresOwnDomainMailAsTheUsers proves that an address
// at a configured own domain is the user's in both directions, whichever of
// To, Cc and Bcc holds the user or the contact.
func TestGmailOwnDomain_SyncStoresOwnDomainMailAsTheUsers(t *testing.T) {
	t.Parallel()
	e := newOwnAddressEnv(t)
	account := e.prefix + "me@synthetic.example"
	ownDomain := e.prefix + "own-domain.example"
	alias := "alias@" + ownDomain
	stranger := e.prefix + "stranger@synthetic.example"
	state := e.emailAccount(t, account, []string{ownDomain}, nil)

	inTo, inToAddr := e.contact(t, "")
	inCc, inCcAddr := e.contact(t, "")
	inBcc, inBccAddr := e.contact(t, "")
	outTo, outToAddr := e.contact(t, "")
	outCc, outCcAddr := e.contact(t, "")
	outBcc, outBccAddr := e.contact(t, "")
	inToMsg := e.message(account, factory.GmailAddresses{From: bare(inToAddr), To: bares(alias)})
	inCcMsg := e.message(account, factory.GmailAddresses{From: bare(inCcAddr), To: bares(stranger), Cc: bares(alias)})
	inBccMsg := e.message(account, factory.GmailAddresses{From: bare(inBccAddr), To: bares(stranger), Bcc: bares(alias)})
	outToMsg := e.message(account, factory.GmailAddresses{From: bare(alias), To: bares(outToAddr)})
	outCcMsg := e.message(account, factory.GmailAddresses{From: bare(alias), To: bares(stranger), Cc: bares(outCcAddr)})
	outBccMsg := e.message(account, factory.GmailAddresses{From: bare(alias), To: bares(stranger), Bcc: bares(outBccAddr)})

	e.sync(t, e.provider(t, inToMsg, inCcMsg, inBccMsg, outToMsg, outCcMsg, outBccMsg), state)

	t.Run("a contact's message to an own-domain address in To is inbound", func(t *testing.T) {
		// spec: ING-051.inbound-to-own
		e.requireStored(t, inTo.ID, inToMsg, repository.InteractionDirectionInbound)
	})
	t.Run("a contact's message with an own-domain address only in Cc is inbound", func(t *testing.T) {
		// spec: ING-051.inbound-to-own
		e.requireStored(t, inCc.ID, inCcMsg, repository.InteractionDirectionInbound)
	})
	t.Run("a contact's message with an own-domain address only in Bcc is inbound", func(t *testing.T) {
		// spec: ING-051.inbound-to-own
		e.requireStored(t, inBcc.ID, inBccMsg, repository.InteractionDirectionInbound)
	})
	t.Run("a message from an own-domain address to a contact is outbound", func(t *testing.T) {
		// spec: ING-051.outbound-from-own
		e.requireStored(t, outTo.ID, outToMsg, repository.InteractionDirectionOutbound)
	})
	t.Run("a message from an own-domain address with the contact only in Cc is outbound", func(t *testing.T) {
		// spec: ING-051.outbound-from-own
		e.requireStored(t, outCc.ID, outCcMsg, repository.InteractionDirectionOutbound)
	})
	t.Run("a message from an own-domain address with the contact only in Bcc is outbound", func(t *testing.T) {
		// spec: ING-051.outbound-from-own
		e.requireStored(t, outBcc.ID, outBccMsg, repository.InteractionDirectionOutbound)
	})
}

// TestGmailOwnAddress_SyncStoresOwnAddressMailAsTheUsers proves that a
// configured own address is the user's in both directions, whichever of To,
// Cc and Bcc holds the user or the contact, while another address at its
// domain is not.
func TestGmailOwnAddress_SyncStoresOwnAddressMailAsTheUsers(t *testing.T) {
	t.Parallel()
	e := newOwnAddressEnv(t)
	account := e.prefix + "me@synthetic.example"
	sharedDomain := e.prefix + "shared-domain.example"
	ownAddress := "me@" + sharedDomain
	state := e.emailAccount(t, account, nil, []string{ownAddress})

	sender, senderAddr := e.contact(t, "")
	recipient, recipientAddr := e.contact(t, "")
	ccRecipient, ccRecipientAddr := e.contact(t, "")
	bccSender, bccSenderAddr := e.contact(t, "")
	bccRecipient, bccRecipientAddr := e.contact(t, "")
	stranger := e.prefix + "stranger@synthetic.example"
	// The colleague shares the own address's domain and is a contact.
	colleague, colleagueAddr := e.contact(t, e.prefix+"colleague@"+sharedDomain)
	colleagueRecipient, colleagueRecipientAddr := e.contact(t, "")

	inMsg := e.message(account, factory.GmailAddresses{From: bare(senderAddr), To: bares(ownAddress)})
	outMsg := e.message(account, factory.GmailAddresses{From: bare(ownAddress), To: bares(recipientAddr)})
	outCcMsg := e.message(account, factory.GmailAddresses{From: bare(ownAddress), To: bares(stranger), Cc: bares(ccRecipientAddr)})
	inBccMsg := e.message(account, factory.GmailAddresses{From: bare(bccSenderAddr), To: bares(stranger), Bcc: bares(ownAddress)})
	outBccMsg := e.message(account, factory.GmailAddresses{From: bare(ownAddress), To: bares(stranger), Bcc: bares(bccRecipientAddr)})
	colleagueMsg := e.message(account, factory.GmailAddresses{From: bare(colleagueAddr), To: bares(colleagueRecipientAddr), Cc: bares(ownAddress)})

	e.sync(t, e.provider(t, inMsg, outMsg, outCcMsg, inBccMsg, outBccMsg, colleagueMsg), state)

	t.Run("a contact's message to the own address is inbound", func(t *testing.T) {
		// spec: ING-051.inbound-to-own
		e.requireStored(t, sender.ID, inMsg, repository.InteractionDirectionInbound)
	})
	t.Run("a message from the own address to a contact is outbound", func(t *testing.T) {
		// spec: ING-051.outbound-from-own
		e.requireStored(t, recipient.ID, outMsg, repository.InteractionDirectionOutbound)
	})
	t.Run("a message from the own address with the contact only in Cc is outbound", func(t *testing.T) {
		// spec: ING-051.outbound-from-own
		e.requireStored(t, ccRecipient.ID, outCcMsg, repository.InteractionDirectionOutbound)
	})
	t.Run("a contact's message with the own address only in Bcc is inbound", func(t *testing.T) {
		// spec: ING-051.inbound-to-own
		e.requireStored(t, bccSender.ID, inBccMsg, repository.InteractionDirectionInbound)
	})
	t.Run("a message from the own address with the contact only in Bcc is outbound", func(t *testing.T) {
		// spec: ING-051.outbound-from-own
		e.requireStored(t, bccRecipient.ID, outBccMsg, repository.InteractionDirectionOutbound)
	})
	t.Run("a colleague's message at the same domain, with the own address only in Cc, is the colleague's inbound mail, never the user's outbound mail", func(t *testing.T) {
		// spec: ING-051.shared-domain-not-own
		// spec: ING-051.inbound-to-own
		e.requireStored(t, colleague.ID, colleagueMsg, repository.InteractionDirectionInbound)
		e.requireNothingStored(t, colleagueRecipient.ID)
	})
}

// TestGmailOwnAddress_QuotedLocalPartSpaceIsSignificant is the regression for
// an own address compared without the space a quoted local part holds: each
// spelling, unquoted and quoted with a leading space, is the user only as
// configured, and the other spelling is another address at the shared domain.
func TestGmailOwnAddress_QuotedLocalPartSpaceIsSignificant(t *testing.T) {
	t.Parallel()
	e := newOwnAddressEnv(t)
	account := e.prefix + "me@synthetic.example"
	sharedDomain := e.prefix + "shared-domain.example"
	// The factory renders an address whose local part starts with a space
	// quoted, as `" other"@shared-domain.example`.
	unquotedOwn := "alias@" + sharedDomain
	quotedOwn := " other@" + sharedDomain
	state := e.emailAccount(t, account, nil, []string{unquotedOwn, `" other"@` + sharedDomain})

	quotedOfUnquoted, quotedOfUnquotedAddr := e.contact(t, "")
	unquotedOfQuoted, unquotedOfQuotedAddr := e.contact(t, "")
	quoted, quotedAddr := e.contact(t, "")
	quotedOfUnquotedMsg := e.message(account, factory.GmailAddresses{From: bare(" alias@" + sharedDomain), To: bares(quotedOfUnquotedAddr)})
	unquotedOfQuotedMsg := e.message(account, factory.GmailAddresses{From: bare("other@" + sharedDomain), To: bares(unquotedOfQuotedAddr)})
	quotedMsg := e.message(account, factory.GmailAddresses{From: bare(quotedOwn), To: bares(quotedAddr)})

	e.sync(t, e.provider(t, quotedOfUnquotedMsg, unquotedOfQuotedMsg, quotedMsg), state)

	t.Run("the quoted spelling of an unquoted own address is not the user", func(t *testing.T) {
		// spec: ING-051.shared-domain-not-own
		e.requireNothingStored(t, quotedOfUnquoted.ID)
	})
	t.Run("the unquoted spelling of a quoted own address is not the user", func(t *testing.T) {
		// spec: ING-051.shared-domain-not-own
		e.requireNothingStored(t, unquotedOfQuoted.ID)
	})
	t.Run("an own address whose quoted local part starts with a space is the user", func(t *testing.T) {
		// spec: ING-051.outbound-from-own
		e.requireStored(t, quoted.ID, quotedMsg, repository.InteractionDirectionOutbound)
	})
}

// TestGmailOwnAddress_MalformedRecipientCountsOnlyConnectedAccounts is the
// regression for a recipient header holding one malformed entry, which
// net/mail cannot parse as an address list: sync recovers its entries by the
// comma split it has always made, and among them only a connected account's
// address is the user, so a fragment of another recipient's quoted local part
// never counts by an own domain.
func TestGmailOwnAddress_MalformedRecipientCountsOnlyConnectedAccounts(t *testing.T) {
	t.Parallel()
	e := newOwnAddressEnv(t)
	account := e.prefix + "me@synthetic.example"
	ownDomain := e.prefix + "own-domain.example"
	sharedDomain := e.prefix + "shared-domain.example"
	state := e.emailAccount(t, account, []string{ownDomain}, nil)

	// toRaw builds a message from sender whose To header is exactly raw,
	// text the factory's well-formed rendering cannot carry.
	toRaw := func(sender, raw string) factory.GmailMessageSpec {
		msg := e.message(account, factory.GmailAddresses{From: bare(sender), To: bares(e.prefix + "stranger@synthetic.example")})
		for _, h := range msg.Message.Payload.Headers {
			if h.Name == "To" {
				h.Value = raw
			}
		}
		return msg
	}

	embeddedSender, embeddedSenderAddr := e.contact(t, "")
	unterminatedSender, unterminatedSenderAddr := e.contact(t, "")
	embeddedMsg := toRaw(embeddedSenderAddr, `"a,alias@`+ownDomain+`,b"@`+sharedDomain+" trailing")
	unterminatedMsg := toRaw(unterminatedSenderAddr, `"broken, `+account)

	e.sync(t, e.provider(t, embeddedMsg, unterminatedMsg), state)

	t.Run("an own-domain address inside a quoted local part, followed by a malformed entry, is not the user", func(t *testing.T) {
		// spec: ING-051.unparsed-header-not-own
		e.requireNothingStored(t, embeddedSender.ID)
	})
	t.Run("a connected account's address after an unterminated quoted string is the user", func(t *testing.T) {
		// spec: ING-051.unparsed-header-not-own
		e.requireStored(t, unterminatedSender.ID, unterminatedMsg, repository.InteractionDirectionInbound)
	})
}

// TestGmailOwnDomain_ConfigOnOneAccountCountsForAnother proves that the
// configuration of every enabled email account makes up the me-set, whichever
// account's mailbox is synced.
func TestGmailOwnDomain_ConfigOnOneAccountCountsForAnother(t *testing.T) {
	t.Parallel()
	e := newOwnAddressEnv(t)
	accountA := e.prefix + "me-a@synthetic.example"
	accountB := e.prefix + "me-b@synthetic.example"
	ownDomain := e.prefix + "own-domain.example"
	ownAddress := "me@" + e.prefix + "shared-domain.example"
	e.emailAccount(t, accountA, []string{ownDomain}, []string{ownAddress})
	stateB := e.emailAccount(t, accountB, nil, nil)

	toDomain, toDomainAddr := e.contact(t, "")
	fromAddress, fromAddressAddr := e.contact(t, "")
	outMsg := e.message(accountB, factory.GmailAddresses{From: bare("alias@" + ownDomain), To: bares(toDomainAddr)})
	inMsg := e.message(accountB, factory.GmailAddresses{From: bare(fromAddressAddr), To: bares(ownAddress)})

	e.sync(t, e.provider(t, outMsg, inMsg), stateB)

	t.Run("an own domain configured on account A counts in account B's mailbox", func(t *testing.T) {
		// spec: ING-051.every-account-configuration
		e.requireStored(t, toDomain.ID, outMsg, repository.InteractionDirectionOutbound)
	})
	t.Run("an own address configured on account A counts in account B's mailbox", func(t *testing.T) {
		// spec: ING-051.every-account-configuration
		e.requireStored(t, fromAddress.ID, inMsg, repository.InteractionDirectionInbound)
	})
}

// TestGmailOwnDomain_RematchScanClassifiesByTheSameAddresses proves that the
// identifier rematch scan classifies by the same me-set as sync: mail from or
// to an own-domain address or an own address, with the rematched address on
// the other side, is the user's in both directions.
func TestGmailOwnDomain_RematchScanClassifiesByTheSameAddresses(t *testing.T) {
	t.Parallel()
	e := newOwnAddressEnv(t)
	account := e.prefix + "me@synthetic.example"
	alias := "alias@" + e.prefix + "own-domain.example"
	ownAddress := "me@" + e.prefix + "shared-domain.example"
	e.emailAccount(t, account, []string{e.prefix + "own-domain.example"}, []string{ownAddress})

	// rematch seeds a contact, scans the message between own and the
	// contact's address, outbound or inbound, and asserts its direction.
	rematch := func(t *testing.T, own, direction string) {
		t.Helper()
		contact, addr := e.contact(t, "")
		addrs := factory.GmailAddresses{From: bare(own), To: bares(addr)}
		if direction == repository.InteractionDirectionInbound {
			addrs = factory.GmailAddresses{From: bare(addr), To: bares(own)}
		}
		msg := e.message(account, addrs)
		_, err := google.NewGmailRematchHandler(e.provider(t, msg), e.syncRepo, e.h.CommsRepo()).Rematch(e.ctx, contact.ID, addr)
		require.NoError(t, err)
		require.NoError(t, e.h.DrainGateB(e.ctx))
		e.requireStored(t, contact.ID, msg, direction)
	}

	t.Run("mail from an own-domain address to the rematched address is outbound", func(t *testing.T) {
		// spec: ING-051.rematch-same-rule
		rematch(t, alias, repository.InteractionDirectionOutbound)
	})
	t.Run("mail from the rematched address to an own-domain address is inbound", func(t *testing.T) {
		// spec: ING-051.rematch-same-rule
		rematch(t, alias, repository.InteractionDirectionInbound)
	})
	t.Run("mail from an own address to the rematched address is outbound", func(t *testing.T) {
		// spec: ING-051.rematch-same-rule
		rematch(t, ownAddress, repository.InteractionDirectionOutbound)
	})
	t.Run("mail from the rematched address to an own address is inbound", func(t *testing.T) {
		// spec: ING-051.rematch-same-rule
		rematch(t, ownAddress, repository.InteractionDirectionInbound)
	})
}

// TestGmailOwnAddress_DiscoveryTreatsOwnAddressesAsTheUser proves that own
// domains and own addresses configured on one account anchor discovery trust
// in another account's mailbox, recorded with the sender's address, and that
// an own participant is never proposed by either discovery source: with or
// without a display name, on a message from the user or from a contact, even
// when its display name matches a contact as a named stranger's does.
//
// spec: IMP-051.own-sender-anchors-trust, IMP-051.own-participant-never-proposed
func TestGmailOwnAddress_DiscoveryTreatsOwnAddressesAsTheUser(t *testing.T) {
	t.Parallel()
	e := newOwnAddressEnv(t)
	accountA := e.prefix + "me-a@synthetic.example"
	accountB := e.prefix + "me-b@synthetic.example"
	ownDomain := e.prefix + "own-domain.example"
	alias := "alias@" + ownDomain
	ownAddress := e.prefix + "me@shared-domain.example"
	namedAlias := "named@" + ownDomain
	namelessAlias := "nameless@" + ownDomain
	guestA := matching.NormalizeEmail(e.prefix + "guest-a@synthetic.example")
	guestB := matching.NormalizeEmail(e.prefix + "guest-b@synthetic.example")
	guestC := matching.NormalizeEmail(e.prefix + "guest-c@synthetic.example")
	namedStranger := matching.NormalizeEmail(e.prefix + "named-stranger@synthetic.example")
	e.emailAccount(t, accountA, []string{ownDomain}, []string{ownAddress})
	stateB := e.emailAccount(t, accountB, nil, nil)

	// Sync searches only mail involving a known contact, so each message
	// carries one. Each named participant displays the full name of a
	// contact it is not, which the correspondence source matches.
	_, known := e.contact(t, "")
	aliasNamesake, _ := e.contact(t, "")
	addressNamesake, _ := e.contact(t, "")
	strangerNamesake, _ := e.contact(t, "")

	fromOwnDomain := e.message(accountB, factory.GmailAddresses{From: bare(alias), To: []factory.GmailAddress{
		bare(known), bare(guestA), bare(namelessAlias),
		{Name: aliasNamesake.FullName, Email: namedAlias},
		{Name: addressNamesake.FullName, Email: ownAddress},
		{Name: strangerNamesake.FullName, Email: namedStranger},
	}})
	fromOwnAddress := e.message(accountB, factory.GmailAddresses{From: bare(ownAddress), To: bares(known, guestB, namelessAlias)})
	fromContact := e.message(accountB, factory.GmailAddresses{From: bare(known), To: bares(guestC, namelessAlias, ownAddress)})

	p := e.provider(t, fromOwnDomain, fromOwnAddress, fromContact)
	externalRepo := repository.NewExternalContactRepository(e.h.Database().Queries)
	p.SetCorrespondenceDiscoverer(google.NewCorrespondenceDiscoverer(e.h.ContactRepo(), externalRepo))
	e.sync(t, p, stateB)

	for _, c := range []struct {
		guest, sender string
		self          bool
	}{
		{guestA, alias, true},
		{guestB, ownAddress, true},
		{guestC, known, false},
	} {
		row, err := externalRepo.GetBySource(e.ctx, google.ParticipantSource, c.guest, nil)
		require.NoError(t, err)
		require.NotNil(t, row, "a trusted sender's recipient is a candidate")
		trustedSender, ok := row.Metadata["trusted_sender"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, matching.NormalizeEmail(c.sender), trustedSender["address"], "the trusted sender's address")
		if c.self {
			require.Equal(t, true, trustedSender["self"], "the sender is the user")
		}
	}

	row, err := externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, namedStranger, nil)
	require.NoError(t, err)
	require.NotNil(t, row, "a named stranger whose name matches a contact is a correspondence candidate")

	for _, own := range []string{namedAlias, namelessAlias, ownAddress} {
		for _, source := range []string{google.ParticipantSource, google.CorrespondenceSource} {
			row, err := externalRepo.GetBySource(e.ctx, source, matching.NormalizeEmail(own), nil)
			require.NoError(t, err)
			require.Nil(t, row, "an address the me-set holds is never proposed")
		}
	}
}

// TestGmailMeSet_ReadsConnectedAccountsFromOAuth proves that MeSet's
// connected-account addresses come from the Google credentials stored in
// PostgreSQL, read through the real OAuthService, beside the configuration of
// the enabled email sync states. The other tests inject the connected
// addresses through the harness; this is the read they stand in for.
func TestGmailMeSet_ReadsConnectedAccountsFromOAuth(t *testing.T) {
	t.Parallel()
	e := newOwnAddressEnv(t)
	accountA := e.prefix + "me-a@synthetic.example"
	accountB := e.prefix + "me-b@synthetic.example"
	ownDomain := e.prefix + "own-domain.example"
	e.emailAccount(t, accountA, []string{ownDomain}, nil)

	oauthRepo := repository.NewOAuthRepository(e.h.Database().Queries)
	for _, account := range []string{accountA, accountB} {
		_, err := oauthRepo.Upsert(e.ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             google.ProviderName,
			AccountID:            account,
			AccessTokenEncrypted: []byte("unused"),
			EncryptionNonce:      []byte("unused"),
			TokenType:            "Bearer",
		})
		require.NoError(t, err)
	}
	oauthSvc, err := google.NewOAuthService(config.TestConfig(), oauthRepo, e.syncRepo)
	require.NoError(t, err)

	meSet, err := google.NewGmailSyncProvider(oauthSvc, e.h.CommsRepo(), e.syncRepo, nil, e.h.Database().Pool).MeSet(e.ctx)
	require.NoError(t, err)
	contains := func(addr string) bool {
		return meSet.Contains(matching.NormalizeEmail(addr), &mail.Address{Address: addr})
	}
	require.True(t, contains(accountA), "a connected account's address")
	require.True(t, contains(accountB), "a connected account with no configuration")
	require.True(t, contains("alias@"+ownDomain), "an address at a configured own domain")
	require.False(t, contains(e.prefix+"stranger@synthetic.example"), "a stranger")
}
