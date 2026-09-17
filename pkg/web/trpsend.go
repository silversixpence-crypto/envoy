package web

// Outgoing Travel Rule Protocol handlers.
//
// SendTRP delivers the first message of a transfer (the inquiry) and SendTRPResolution
// delivers the compliance decision (accept/reject) back to the originator over the
// TRP callback. Both store their envelopes themselves because TRP messages are
// plaintext on the wire and have to be sealed with the local storage key instead of
// the counterparty's public key.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/trisacrypto/envoy/pkg/postman"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/trisa/pkg/openvasp/client"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	"github.com/trisacrypto/trisa/pkg/trisa/envelope"
	"github.com/trisacrypto/trisa/pkg/trisa/keys"
)

const (
	trpTransfersPath = "/transfers"
	trpResolvePath   = "resolve"

	// The TRP version advertised on outgoing requests. The trisa library still
	// reports 3.1.0, but the payloads this node produces follow the 3.2 wire format
	// (integer base-unit amounts) and 21 Analytics' server runs 3.2.1 and returns a
	// bare 400 to versions it does not accept.
	trpAPIVersion = "3.2.1"
)

var ErrNoTRPEndpoint = errors.New("counterparty does not have a trp endpoint")

// SendTRP posts an outgoing travel rule inquiry to the counterparty's TRP endpoint,
// records the returned resolution as the incoming message, and stores both envelopes.
func (s *Server) SendTRP(ctx context.Context, p *postman.TRPPacket) (err error) {
	var endpoint *url.URL

	if endpoint, err = trpEndpoint(p.Counterparty); err != nil {
		return err
	}

	var inquiry *trp.Inquiry

	if inquiry, err = postman.InquiryFromPayload(p.Payload()); err != nil {
		return err
	}

	envelopeID := p.EnvelopeID().String()

	inquiry.Callback = s.trpCallback(endpoint.Scheme, envelopeID)
	inquiry.Info = &trp.Info{
		Address:           endpoint.String(),
		APIVersion:        trpAPIVersion,
		RequestIdentifier: envelopeID,
	}

	if err = inquiry.Validate(); err != nil {
		return fmt.Errorf("could not create a valid trp inquiry: %w", err)
	}

	var trpc *client.Client

	if trpc, err = client.New(); err != nil {
		return err
	}

	p.Log.Debug().Str("endpoint", endpoint.String()).Msg("sending outgoing trp inquiry")

	var resolution *trp.Resolution

	if resolution, err = trpc.Inquiry(ctx, inquiry); err != nil {
		p.Log.Error().Err(err).Str("endpoint", endpoint.String()).Msg("could not send trp inquiry to counterparty")

		return ErrUnavailable
	}

	if err = p.ReceiveResolution(resolution); err != nil {
		return err
	}

	return s.storeTRPEnvelopes(&p.Packet, "Server.SendTRP()")
}

// SendTRPResolution posts the local compliance decision for an inbound TRP transfer
// back to the originator, then records an echo of the decision as the incoming
// message so that the transaction reaches its terminal status locally.
func (s *Server) SendTRPResolution(ctx context.Context, p *postman.TRISAPacket) (err error) {
	var endpoint *url.URL

	if endpoint, err = trpEndpoint(p.Counterparty); err != nil {
		return err
	}

	envelopeID := p.EnvelopeID()
	resolution := &trp.Resolution{}

	switch state := p.Out.Envelope.TransferState(); state {
	case trisa.TransferAccepted, trisa.TransferCompleted:
		resolution.Approved = &trp.Approval{
			Address:  postman.BeneficiaryAddress(p.Out.Envelope),
			Callback: s.trpCallback(endpoint.Scheme, envelopeID),
		}

		// TRP requires a payment address on an approval; fall back to the envelope
		// id so a transfer without a beneficiary wallet is still resolvable.
		if resolution.Approved.Address == "" {
			resolution.Approved.Address = envelopeID
		}
	case trisa.TransferRejected, trisa.TransferRepair:
		resolution.Rejected = rejectionComment(p.Out.Envelope)
	default:
		return fmt.Errorf("cannot send a trp resolution for the %q transfer state", state.String())
	}

	if err = resolution.Validate(); err != nil {
		return fmt.Errorf("could not create a valid trp resolution: %w", err)
	}

	// The counterparty endpoint may carry a path prefix (multi-tenant deployments
	// route /<client>/transfers through a single hostname), so the resolve path is
	// appended to the endpoint's path rather than replacing it.
	callback := *endpoint
	callback.Path = strings.TrimSuffix(strings.TrimSuffix(callback.Path, "/"), trpTransfersPath) +
		strings.Join([]string{trpTransfersPath, envelopeID, trpResolvePath}, "/")

	resolution.Info = &trp.Info{
		Address:           callback.String(),
		APIVersion:        trpAPIVersion,
		RequestIdentifier: envelopeID,
	}

	var trpc *client.Client

	if trpc, err = client.New(); err != nil {
		return err
	}

	p.Log.Debug().Str("callback", callback.String()).Msg("sending outgoing trp resolution")

	if err = trpc.Resolve(ctx, resolution); err != nil {
		p.Log.Error().Err(err).Str("callback", callback.String()).Msg("could not send trp resolution to counterparty")

		return ErrUnavailable
	}

	if err = p.EchoIncoming(); err != nil {
		return err
	}

	return s.storeTRPEnvelopes(&p.Packet, "Server.SendTRPResolution()")
}

// storeTRPEnvelopes seals both halves of a TRP packet with the node's storage key and
// writes them to the database, outgoing first so the incoming reply can point at it.
func (s *Server) storeTRPEnvelopes(p *postman.Packet, notes string) (err error) {
	var storageKey keys.PublicKey

	if storageKey, err = s.trisa.StorageKey("", p.Counterparty.CommonName); err != nil {
		return fmt.Errorf("could not fetch storage key: %w", err)
	}

	clear := p.In.Envelope

	if err = p.SealLocal(storageKey); err != nil {
		return err
	}

	if err = p.DB.AddEnvelope(p.Out.Model(), &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: notes},
	}); err != nil {
		return fmt.Errorf("could not store outgoing envelope: %w", err)
	}

	if err = p.DB.AddEnvelope(p.In.Model(), &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: notes},
	}); err != nil {
		return fmt.Errorf("could not store incoming envelope: %w", err)
	}

	p.RevealIncoming(clear)

	return nil
}

// trpCallback builds the URL that a counterparty should post resolutions and
// confirmations for this transfer back to. The scheme mirrors the one used to reach
// the counterparty so plaintext lab deployments stay plaintext in both directions.
//
// Multi-tenant deployments configure TRISA_TRP_ENDPOINT as host/<prefix> so that a
// path-routing proxy in front of a single public hostname can find this node again;
// the prefix is carried into every callback.
func (s *Server) trpCallback(scheme, envelopeID string) string {
	endpoint := strings.TrimPrefix(strings.TrimPrefix(s.conf.TRP.Endpoint, "https://"), "http://")
	host, prefix, _ := strings.Cut(endpoint, "/")

	uri := &url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   trpTransfersPath + "/" + envelopeID,
	}

	if prefix != "" {
		uri.Path = "/" + strings.TrimSuffix(prefix, "/") + uri.Path
	}

	return uri.String()
}

// trpEndpoint resolves the URL that TRP requests for a counterparty should be sent
// to. Endpoints stored without a scheme are assumed to be https as the TRP spec
// requires; lab and demo deployments store an explicit http:// scheme.
func trpEndpoint(cp *models.Counterparty) (uri *url.URL, err error) {
	if cp == nil || cp.Endpoint == "" {
		return nil, ErrNoTRPEndpoint
	}

	endpoint := cp.Endpoint

	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}

	if uri, err = url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("could not parse counterparty trp endpoint: %w", err)
	}

	if uri.Host == "" {
		return nil, ErrNoTRPEndpoint
	}

	if uri.Path == "" || uri.Path == "/" {
		uri.Path = trpTransfersPath
	}

	uri.RawQuery = ""

	return uri, nil
}

// rejectionComment extracts a human readable comment from a TRISA error envelope for
// the rejected field of a TRP resolution.
func rejectionComment(env *envelope.Envelope) string {
	if env != nil {
		if reject := env.Error(); reject != nil && reject.Message != "" {
			return reject.Message
		}
	}

	return "the counterparty rejected this transfer"
}
