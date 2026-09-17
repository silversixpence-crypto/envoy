package postman

// First contact handling for inbound Travel Rule Protocol inquiries.
//
// Upstream Envoy refuses an inbound TRP inquiry whenever it cannot resolve the sender
// to a counterparty it already holds: TRPPacket.ResolveCounterparty returns
// ErrNoCounterpartyInfo and the transfer is never created. That assumes every peer is
// discoverable in advance — through the TRISA directory, a synced counterparty list,
// or an mTLS certificate chain the node already trusts.
//
// A directory-less TRP deployment has none of those. There is no global registry of
// TRP endpoints to sync from, so the platform cannot pre-seed every VASP that might
// one day send to one of its nodes, and refusing first contact means the very first
// message from a legitimate peer is always dropped. Instead the sender is recorded
// from the inquiry itself as an unvetted counterparty with source "peer", which is
// exactly how Envoy already labels a counterparty learned from a peer rather than
// from a directory. Nothing about that record is trusted: the transfer lands in the
// inbox in review status for a compliance officer to accept or reject, and they see
// the source of the record alongside it.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	"github.com/rs/zerolog"
	"github.com/trisacrypto/envoy/pkg/enum"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/trisa/pkg/ivms101"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
)

var ErrNoTRPCallback = errors.New("trp inquiry has no callback url to identify the sender with")

// peerCounterpartyFromInquiry builds an unsaved counterparty record for a TRP sender
// the node has never seen before. The callback URL is the only identifier a TRP
// inquiry is guaranteed to carry, so its host becomes the common name and endpoint;
// the IVMS101 originating VASP, when the inquiry supplies one, provides the legal
// name, the country and the stored identity record.
//
// The originating VASP's legal person is kept whole and stored exactly as it arrived,
// including when it does not satisfy ivms101 validation: an auto-registered sender
// should end up with the same identity record a manually registered one would have,
// and dropping it leaves the counterparty's ivms101 column null with nothing to
// review. A validation failure is reported through log instead of discarding the
// record. The returned record is not persisted here: the caller sets it on the packet
// and Incoming.UpdateTransaction stores it through AddCounterparty.
func peerCounterpartyFromInquiry(inquiry *trp.Inquiry, log zerolog.Logger) (counterparty *models.Counterparty, err error) {
	if inquiry == nil || inquiry.Callback == "" {
		return nil, ErrNoTRPCallback
	}

	var uri *url.URL

	if uri, err = url.Parse(inquiry.Callback); err != nil {
		return nil, fmt.Errorf("could not parse trp callback url: %w", err)
	}

	if uri.Host == "" {
		return nil, ErrNoTRPCallback
	}

	// Keep the callback's scheme: production peers are https but TRP does not require
	// TLS, and rewriting the scheme would point the endpoint at a port that is closed.
	scheme := uri.Scheme

	if scheme == "" {
		scheme = "https"
	}

	// The endpoint is the origin of the callback only; the path is specific to the
	// transfer the callback belongs to.
	origin := &url.URL{Scheme: scheme, Host: uri.Host}

	counterparty = &models.Counterparty{
		Source:     enum.SourcePeer,
		Protocol:   enum.ProtocolTRP,
		CommonName: uri.Hostname(),
		Endpoint:   origin.String(),
		Name:       uri.Hostname(),
	}

	vasp := originatingVASP(inquiry)

	if vasp == nil {
		return counterparty, nil
	}

	// Store the legal person as received, whether or not it validates.
	counterparty.IVMSRecord = vasp

	if verr := vasp.Validate(); verr != nil {
		log.Warn().Err(verr).Msg("originating vasp ivms101 record failed validation; stored as received")
	}

	if name := legalPersonName(vasp); name != "" {
		counterparty.Name = name
	}

	if vasp.CountryOfRegistration != "" {
		counterparty.Country = sql.NullString{Valid: true, String: vasp.CountryOfRegistration}
	}

	return counterparty, nil
}

// originatingVASP returns the legal person of the originating VASP on an inquiry's
// identity payload, or nil if the inquiry does not carry one.
func originatingVASP(inquiry *trp.Inquiry) *ivms101.LegalPerson {
	if inquiry.IVMS101 == nil || inquiry.IVMS101.OriginatingVasp == nil {
		return nil
	}

	if inquiry.IVMS101.OriginatingVasp.OriginatingVasp == nil {
		return nil
	}

	return inquiry.IVMS101.OriginatingVasp.OriginatingVasp.GetLegalPerson()
}

// legalPersonName extracts the display name of a legal person, preferring the legal
// name over trading or short names as the rest of Envoy does.
func legalPersonName(vasp *ivms101.LegalPerson) string {
	if vasp.Name == nil || len(vasp.Name.NameIdentifiers) == 0 {
		return ""
	}

	for _, name := range vasp.Name.NameIdentifiers {
		if name.LegalPersonNameIdentifierType == ivms101.LegalPersonLegal && name.LegalPersonName != "" {
			return name.LegalPersonName
		}
	}

	return vasp.Name.NameIdentifiers[0].LegalPersonName
}
