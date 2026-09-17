package trp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/trisacrypto/envoy/pkg/logger"
	"github.com/trisacrypto/envoy/pkg/postman"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/envoy/pkg/trp/callback"
	"github.com/trisacrypto/envoy/pkg/webhook"
	"github.com/trisacrypto/trisa/pkg/ivms101"
	"github.com/trisacrypto/trisa/pkg/openvasp"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	generic "github.com/trisacrypto/trisa/pkg/trisa/data/generic/v1beta1"
	"github.com/trisacrypto/trisa/pkg/trisa/keys"
)

func (s *Server) Inquiry(c *gin.Context) {
	var (
		err    error
		in     *trp.Inquiry
		out    *trp.Resolution
		packet *postman.TRPPacket
	)

	in = &trp.Inquiry{
		Info: TRPInfo(c),
	}

	ctx := c.Request.Context()
	log := logger.Tracing(ctx).With().
		Str("request_identifier", in.Info.RequestIdentifier).
		Str("api_version", in.Info.APIVersion).
		Strs("api_extensions", in.Info.APIExtensions).
		Logger()

	// Parse and validate the JSON inquiry
	if err = c.BindJSON(in); err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	log.Debug().
		Str("address", in.Info.Address).
		Msg("processing incoming trp inquiry")

	if err = in.Validate(); err != nil {
		c.AbortWithError(http.StatusUnprocessableEntity, err)
		return
	}

	// if err = in.IVMS101.Validate(); err != nil {
	// 	c.AbortWithError(http.StatusUnprocessableEntity, err)
	// 	return
	// }

	if packet, err = postman.ReceiveTRPInquiry(in, c.Request.TLS); err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	packet.Log = log
	if packet.DB, err = s.store.PrepareTransaction(c.Request.Context(), packet.EnvelopeID(), &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.Inquiry()"},
	}); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	// Rollback the prepared transaction if there are any errors in processing
	defer packet.DB.Rollback()

	// Create the transaction from the payload
	packet.Transaction = postman.TransactionFromPayload(packet.Payload())

	// Update the transaction record and add counterparty information and status
	// TODO: this may return an invalid counterparty error, which should return a different status error
	if err = packet.In.UpdateTransaction(); err != nil {
		log.Warn().Err(err).Bool("stored_to_database", false).Msg("could not update transaction details and counterparty information")
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	// TODO: load auto approve/reject policies for counterparty to determine response

	// Determine how to construct a response back to the remote counterparty; e.g. by
	// using the webhook, using an automated policy, or making an automatic response
	// determined by the transfer state .
	switch {
	case s.WebhookEnabled():
		if out, err = s.WebhookInquiry(ctx, packet); err != nil {
			// An unreachable compliance callback is a temporary condition on this node,
			// not a problem with the inquiry: answer 503 so the peer retries rather than
			// storing a transfer that was never reviewed.
			if errors.Is(err, ErrWebhookUnavailable) {
				log.Error().Err(err).Bool("stored_to_database", false).Msg("compliance webhook unavailable for incoming trp inquiry")
				c.AbortWithError(http.StatusServiceUnavailable, err)

				return
			}

			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	default:
		out = &trp.Resolution{
			Version: openvasp.APIVersion,
		}
	}

	// Handle the outgoing message
	if err = packet.Resolve(out); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not resolve outgoing trp inquiry")
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	// Get the storage key and seal the envelope
	// TODO: handle the secure-trisa-envelope case where encryption is required
	var storageKey keys.PublicKey
	if storageKey, err = s.trisa.StorageKey("", packet.CommonName()); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not get storage key for trp inquiry")
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	if err = packet.Seal(storageKey); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not seal trp inquiry")
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	// Store Incoming Message
	if err = packet.DB.AddEnvelope(packet.In.Model(), &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.Inquiry()"},
	}); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not store incoming trp inquiry in database")
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	// Store Outgoing message
	if err = packet.DB.AddEnvelope(packet.Out.Model(), &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.Inquiry()"},
	}); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not store outgoing trp inquiry in database")
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	// Update the transaction with the outgoing message info
	if err = packet.Out.UpdateTransaction(); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not update transaction with outgoing info in database")
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	// Commit the transaction to the database (success!)
	if err = packet.DB.Commit(); err != nil {
		log.Warn().Err(err).Bool("stored_to_database", false).Msg("could not commit incoming trisa transfer to database")
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	log.Info().Bool("stored_to_database", true).Msg("incoming trp inquiry handling complete")
	c.JSON(http.StatusOK, out)
}

// NOTE: Server.Resolve is implemented in resolve.go and Server.Confirmation in
// confirm.go so that inbound callbacks update the transaction they refer to.

// Get the TRP info from the context as set by the VerifyTRPCore middleware.
func TRPInfo(c *gin.Context) *trp.Info {
	info := &trp.Info{
		Address: c.Request.URL.String(),
	}

	if val, ok := c.Get(ctxAPIVersionKey); ok {
		info.APIVersion = val.(string)
	}

	if val, ok := c.Get(ctxIdentifierKey); ok {
		info.RequestIdentifier = val.(string)
	}

	if val, ok := c.Get(ctxExtensionsKey); ok {
		info.APIExtensions = strings.Split(val.(string), ",")
		for i, val := range info.APIExtensions {
			info.APIExtensions[i] = strings.TrimSpace(val)
		}
	}

	return info
}

//===========================================================================
// Webhook Interactions
//===========================================================================

// WebhookEnabled reports whether inbound TRP messages should be posted to the compliance
// callback, which is the case whenever a webhook handler was configured for the node.
func (s *Server) WebhookEnabled() bool {
	return s.webhook != nil
}

// WebhookInquiry posts an inbound inquiry to the compliance callback and turns the reply
// into the TRP resolution that answers the counterparty synchronously. It mirrors the
// TRISA server's WebhookResponse, with the differences that TRP forces: the reply has to
// become an approval, a rejection or an acknowledgement rather than an arbitrary payload,
// and an approval is only valid if a payment address can be supplied with it.
func (s *Server) WebhookInquiry(ctx context.Context, packet *postman.TRPPacket) (out *trp.Resolution, err error) {
	// A version-only resolution tells the counterparty "received, decision to follow",
	// which is what every reply that is not a clear accept or reject falls back to.
	pending := &trp.Resolution{
		Version: openvasp.APIVersion,
	}

	request := packet.In.WebhookRequestFor(webhook.ProtocolTRP)

	if err = request.AddPayload(packet.Payload()); err != nil {
		packet.Log.Error().Err(err).Msg("could not add payload to webhook callback")
		return nil, fmt.Errorf("could not add trp payload to webhook callback: %w", err)
	}

	var reply *webhook.Reply

	if reply, err = s.webhook.Callback(ctx, request); err != nil {
		packet.Log.Error().Err(err).Msg("could not execute webhook callback")
		return nil, ErrWebhookUnavailable
	}

	// If a 204 no content response is received, then acknowledge the inquiry and leave
	// the transfer for the compliance team to resolve out of band.
	if reply.TransferAction == webhook.DefaultTransferAction {
		packet.Log.Debug().Msg("received 204 no content from webhook callback, acknowledging trp inquiry")
		return pending, nil
	}

	// Sanity check the transaction id the callback replied about.
	if reply.TransactionID != request.TransactionID {
		packet.Log.Error().Msg("reply/request transaction id mismatch")
		return nil, errors.New("webhook reply transaction id does not match the request")
	}

	if reply.Error != nil {
		comment := reply.Error.Message

		if comment == "" {
			comment = defaultRejection
		}

		return &trp.Resolution{Rejected: comment}, nil
	}

	if reply.TransferState() != trisa.TransferAccepted {
		return pending, nil
	}

	address := replyBeneficiaryAddress(reply, packet.Payload())

	// TRP has no way to express "approved, address to follow": an approval without a
	// payment address is not a message the originator can act on, so fall back to the
	// acknowledgement and let the compliance team resolve the transfer once the account
	// has an address on it.
	if address == "" {
		packet.Log.Warn().Msg("compliance callback approved a trp inquiry without a beneficiary payment address; acknowledging instead")
		return pending, nil
	}

	return &trp.Resolution{
		Approved: &trp.Approval{
			Address:  address,
			Callback: s.callbackURL(packet.EnvelopeID().String(), callback.PurposeConfirm, inquiryCallback(packet.Payload())),
		},
	}, nil
}

// replyBeneficiaryAddress finds the payment address to approve a transfer with: the
// callback may name it on the transaction it replies with, otherwise the address on the
// beneficiary account of the inquiry's own IVMS101 record is used.
func replyBeneficiaryAddress(reply *webhook.Reply, inquiry *trisa.Payload) string {
	if reply.Payload != nil && reply.Payload.Transaction != nil && reply.Payload.Transaction.Beneficiary != "" {
		return reply.Payload.Transaction.Beneficiary
	}

	identity := &ivms101.IdentityPayload{}

	if err := inquiry.Identity.UnmarshalTo(identity); err != nil {
		return ""
	}

	if identity.Beneficiary == nil {
		return ""
	}

	return postman.FindAccount(identity.Beneficiary)
}

// inquiryCallback returns the callback URL the counterparty supplied with its inquiry,
// which is used only to decide whether this node should advertise its own callbacks over
// http or https.
func inquiryCallback(payload *trisa.Payload) string {
	if payload == nil || payload.Transaction == nil {
		return ""
	}

	msg := &generic.TRP{}

	if err := payload.Transaction.UnmarshalTo(msg); err != nil {
		return ""
	}

	return msg.GetInquiry().GetCallback()
}

// callbackURL builds the token bearing callback URL that a counterparty should post the
// next message of this transfer to.
//
// The scheme is taken from the configured TRP endpoint when it names one; otherwise the
// scheme the counterparty used for its own callback is mirrored so that plaintext lab
// deployments stay plaintext in both directions. The TRP specification requires https,
// which is the fallback.
func (s *Server) callbackURL(envelopeID, purpose, peer string) string {
	scheme := "https"

	if prefix, _, ok := strings.Cut(s.conf.TRP.Endpoint, "://"); ok {
		scheme = prefix
	} else if peer != "" {
		if uri, err := url.Parse(peer); err == nil && uri.Scheme != "" {
			scheme = uri.Scheme
		}
	}

	return callback.CallbackURL(s.conf.TRP.Endpoint, scheme, envelopeID, purpose, s.conf.TRP.DecodeCallbackKey())
}
