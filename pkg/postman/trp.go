package postman

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/trisacrypto/envoy/pkg/enum"
	"github.com/trisacrypto/envoy/pkg/store/models"
	trpclient "github.com/trisacrypto/envoy/pkg/trp/client"
	"github.com/trisacrypto/trisa/pkg/openvasp/client"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	"github.com/trisacrypto/trisa/pkg/trisa/envelope"
	"github.com/trisacrypto/trisa/pkg/trisa/keys"
)

type TRPPacket struct {
	Packet
	info       *trp.Info
	mtls       *tls.ConnectionState
	payload    *trisa.Payload
	message    interface{}
	envelopeID uuid.UUID
}

func ReceiveTRPInquiry(inquiry *trp.Inquiry, mtls *tls.ConnectionState) (packet *TRPPacket, err error) {
	packet = &TRPPacket{
		Packet: Packet{
			In:      &Incoming{},
			Out:     &Outgoing{},
			request: enum.DirectionIncoming,
			reply:   enum.DirectionOutgoing,
		},
		info:    inquiry.Info,
		mtls:    mtls,
		message: inquiry,
	}

	// Make sure the info has a request identifier
	if inquiry.Info.RequestIdentifier == "" {
		return nil, ErrNoRequestIdentifier
	}

	// Make sure the request identifier is a parseable UUID for the database
	if packet.envelopeID, err = uuid.Parse(inquiry.Info.RequestIdentifier); err != nil {
		return nil, ErrInvalidUUID
	}

	// Add parent to submessages
	packet.In.packet = &packet.Packet
	packet.Out.packet = &packet.Packet

	// Create the payload from the inquiry
	if packet.payload, err = PayloadFromInquiry(inquiry); err != nil {
		return nil, err
	}

	// Create the incoming envelope
	opts := []envelope.Option{
		envelope.WithEnvelopeID(inquiry.Info.RequestIdentifier),
		envelope.WithTransferState(trisa.TransferStarted),
	}

	if packet.In.Envelope, err = envelope.New(packet.payload, opts...); err != nil {
		return nil, fmt.Errorf("could not create incoming trp inquiry envelope: %w", err)
	}

	packet.In.original = packet.In.Envelope.Proto()
	packet.Packet.resolver = packet
	return packet, nil
}

// ReceiveTRPConfirmation records an inbound TRP confirmation (the originator reporting
// that an approved transfer settled on chain or was canceled) as the incoming half of a
// packet. A confirmation is acknowledged with an empty 204, so unlike an inquiry the
// packet has no outgoing message; base is the payload of the last message stored for the
// transfer, from which the identity and reference transaction are carried over.
func ReceiveTRPConfirmation(envelopeID uuid.UUID, base *trisa.Payload, in *trp.Confirmation, mtls *tls.ConnectionState) (packet *TRPPacket, err error) {
	packet = &TRPPacket{
		Packet: Packet{
			In:      &Incoming{},
			Out:     &Outgoing{},
			request: enum.DirectionIncoming,
			reply:   enum.DirectionOutgoing,
		},
		info:       in.Info,
		mtls:       mtls,
		message:    in,
		envelopeID: envelopeID,
	}

	// Add parent to submessages
	packet.In.packet = &packet.Packet
	packet.Out.packet = &packet.Packet

	if packet.payload, err = PayloadFromConfirmation(base, in); err != nil {
		return nil, err
	}

	transferState := trisa.TransferCompleted

	if in.Canceled != "" {
		transferState = trisa.TransferRejected
	}

	opts := []envelope.Option{
		envelope.WithEnvelopeID(envelopeID.String()),
		envelope.WithTransferState(transferState),
	}

	if packet.In.Envelope, err = envelope.New(packet.payload, opts...); err != nil {
		return nil, fmt.Errorf("could not create incoming trp confirmation envelope: %w", err)
	}

	packet.In.original = packet.In.Envelope.Proto()
	packet.Packet.resolver = packet
	return packet, nil
}

// Resolve prepares the outgoing half of an inbound inquiry from the resolution this node
// is replying with. An approval and a rejection are terminal decisions and are recorded
// as such, so that the transaction reaches accepted or rejected through the ordinary
// Outgoing.UpdateTransaction path; a version-only resolution leaves the transfer pending
// on the local compliance team.
func (p *TRPPacket) Resolve(out *trp.Resolution) (err error) {
	switch {
	case out.Rejected != "":
		// A rejection is stored the way TRISA rejections are, as an error envelope, so
		// that the UI, the API and StatusFromTransferState all treat it identically.
		reject := &trisa.Error{
			Code:    trisa.Rejected,
			Message: out.Rejected,
			Retry:   false,
		}

		if p.Out.Envelope, err = envelope.WrapError(reject, envelope.WithEnvelopeID(p.envelopeID.String())); err != nil {
			p.Log.Debug().Err(err).Msg("could not prepare outgoing rejection")
			return fmt.Errorf("could not create outgoing trp rejection envelope: %w", err)
		}

		return nil

	case out.Approved != nil:
		var payload *trisa.Payload

		if payload, err = PayloadFromResolution(p.payload, out); err != nil {
			return fmt.Errorf("could not create outgoing trp approval payload: %w", err)
		}

		if p.Out.Envelope, err = p.In.Envelope.Update(payload, envelope.WithTransferState(trisa.TransferAccepted)); err != nil {
			p.Log.Debug().Err(err).Msg("could not prepare outgoing payload")
			return fmt.Errorf("could not create outgoing trp resolution envelope: %w", err)
		}

		return nil

	default:
		if p.Out.Envelope, err = p.In.Envelope.Update(p.payload, envelope.WithTransferState(trisa.TransferPending)); err != nil {
			p.Log.Debug().Err(err).Msg("could not prepare outgoing payload")
			return fmt.Errorf("could not create outgoing trp resolution envelope: %w", err)
		}

		return nil
	}
}

func (p *TRPPacket) EnvelopeID() uuid.UUID {
	return p.envelopeID
}

func (p *TRPPacket) Payload() *trisa.Payload {
	return p.payload
}

func (p *TRPPacket) CommonName() string {
	if p.Counterparty != nil {
		return p.Counterparty.CommonName
	}
	return ""
}

// Encrypts an unencrypted incoming TRP inquiry, resolution, or confirmation message
// using the specified storage key to ensure secure envelopes are always encrypted in
// the database. This handles both the incoming and outgoing messages and should not
// be used with the secure-trisa-envelope extension is being used.
func (p *TRPPacket) Seal(storageKey keys.PublicKey) (err error) {
	if storageKey == nil {
		return ErrNoSealingKey
	}

	// A confirmation is acknowledged with an empty 204, so the packet that records one
	// has no outgoing message to seal.
	if p.Out.Envelope != nil && !p.Out.Envelope.IsError() {
		// Ensure the outgoing message has the same encryption key as the incoming!
		p.Out.StorageKey = storageKey
		p.Out.SealingKey = storageKey

		if _, err = p.Out.Seal(); err != nil {
			return fmt.Errorf("could not encrypt outgoing envelope: %w", err)
		}
	}

	if !p.In.Envelope.IsError() {
		if p.In.Envelope, _, err = p.In.Envelope.Encrypt(); err != nil {
			return fmt.Errorf("could not encrypt trp message: %w", err)
		}

		if p.In.Envelope, _, err = p.In.Envelope.Seal(envelope.WithSealingKey(storageKey)); err != nil {
			return fmt.Errorf("could not seal trp message: %w", err)
		}

		// Store the encrypted and sealed envelope as the "original" message, which will
		// be saved in the database when s.In.Model() is called.
		p.In.original = p.In.Envelope.Proto()
	}

	return nil
}

// Returns the remote information for storage in the database using the underlying
// resolver if available, otherwise a NULL string is returned.
func (p *TRPPacket) Remote() sql.NullString {
	commonName := p.CommonName()
	return sql.NullString{Valid: commonName != "", String: commonName}
}

// Resolve counterparty through the following methods:
//
// 1. Try to lookup the counterparty via the mTLS hostname
// 2. Try to lookup the counterparty using the callback hostname
// 3. Try to lookup the counterparty using the originator name
// 4. Try to lookup the counterparty using the beneficiary name
//
// If a hostname is available, perform an identity lookup.
// Note: name matches must be exact; they are not fuzzy searches.
func (p *TRPPacket) ResolveCounterparty() (err error) {
	// Outgoing packets already know which counterparty they are addressed to.
	if p.Counterparty != nil {
		return nil
	}

	// Attempt to resolve the counterparty from the incoming mTLS connection
	if p.mtls != nil {
		if len(p.mtls.PeerCertificates) > 0 {
			cert := p.mtls.PeerCertificates[0]
			hostnames := make([]string, 0, len(cert.DNSNames)+1)
			hostnames = append(hostnames, cert.Subject.CommonName)
			hostnames = append(hostnames, cert.DNSNames...)

			if p.Counterparty, err = p.resolveCounterpartyHostname(hostnames...); err == nil {
				return nil
			}
		}
	}

	if p.payload != nil {
		if inquiry, ok := p.message.(*trp.Inquiry); ok {
			// Attempt to resolve the counterparty from the callback hostname
			if callback := inquiry.Callback; callback != "" {
				if uri, err := url.Parse(callback); err == nil {
					if p.Counterparty, err = p.resolveCounterpartyHostname(uri.Host); err == nil {
						return nil
					}
				}
			}

			// TODO: Attempt to resolve the counterparty from the originator name

			// TODO: Attempt to resolve the counterparty from the beneficiary name
		}
	}

	// If we get to this point and we were unable to resolve a counterparty then only
	// return an error if this transaction is being created (a counterparty is required
	// to associate with the tranaction). Otherwise return no error in the case of an
	// update because of resolution or confirmation.
	if p.DB.Created() {
		// An unknown sender is registered as an unvetted peer counterparty rather
		// than refused; the transfer still lands in the inbox for review.
		if inquiry, ok := p.message.(*trp.Inquiry); ok {
			var counterparty *models.Counterparty

			if counterparty, err = peerCounterpartyFromInquiry(inquiry, p.Log); err != nil {
				p.Log.Warn().Err(err).Msg("could not register unknown trp sender as a peer counterparty")
				return ErrNoCounterpartyInfo
			}

			p.Counterparty = counterparty
			p.Log.Info().Str("common_name", counterparty.CommonName).Str("name", counterparty.Name).Msg("unknown trp sender auto-registered as a peer counterparty")

			return nil
		}

		return ErrNoCounterpartyInfo
	}

	return nil
}

func (p *TRPPacket) resolveCounterpartyHostname(hostnames ...string) (counterparty *models.Counterparty, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var trpClient *client.Client
	if trpClient, err = trpclient.New(); err != nil {
		return nil, err
	}

	for _, host := range hostnames {
		uri := url.URL{Scheme: "https", Host: host}

		// Attempt to lookup the counterparty in the database by common name
		if counterparty, err = p.DB.LookupCounterparty(models.FieldCommonName, uri.Hostname()); err == nil {
			return counterparty, nil
		} else {
			p.Log.Debug().Err(err).Str("hostname", uri.Hostname()).Msg("counterparty not found by hostname")
		}

		// Attempt to lookup the counterparty identity from the host
		var identity *trp.Identity
		if identity, err = trpClient.Identity(ctx, uri.String()); err != nil {
			p.Log.Debug().Err(err).Str("host", host).Msg("could not resolve counterparty identity from host")
			continue
		}

		// Lookup identity in database by LEI
		// This occurs when the hostname is different than the common name stored in the database
		if identity.LEI != "" {
			if counterparty, err = p.DB.LookupCounterparty(models.FieldLEI, identity.LEI); err == nil {
				return counterparty, nil
			} else {
				p.Log.Debug().Err(err).Str("lei", identity.LEI).Msg("counterparty not found by lei")
			}
		}

		// If the identity lookup was successful: create a new counterparty from the returned identity
		p.Log.Debug().Str("host", host).Str("lei", identity.LEI).Str("name", identity.Name).Msg("trp counterparty identity resolved from peer hostname")
		return &models.Counterparty{
			Source:     enum.SourcePeer,
			Protocol:   enum.ProtocolTRP,
			Endpoint:   uri.String(),
			CommonName: uri.Hostname(),
			Name:       identity.Name,
			LEI:        sql.NullString{Valid: identity.LEI != "", String: identity.LEI},
			Country:    sql.NullString{Valid: false},
		}, nil
	}

	return nil, ErrCounterpartyNotFound
}
