package trp

// Inbound TRP confirmation callbacks.
//
// After this node approves an inbound inquiry it hands the originator a confirmation
// callback. The originator posts a trp.Confirmation to it once the transaction settles
// on chain, or to cancel the transfer. Upstream Envoy answered that callback with a bare
// 204 and discarded the body, leaving every approved inbound transfer stuck in accepted;
// this handler applies the confirmation and records it as a secure envelope.

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/trisacrypto/envoy/pkg/enum"
	"github.com/trisacrypto/envoy/pkg/logger"
	"github.com/trisacrypto/envoy/pkg/postman"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/envoy/pkg/trp/callback"
	"github.com/trisacrypto/envoy/pkg/webhook"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	"github.com/trisacrypto/trisa/pkg/trisa/envelope"
	"github.com/trisacrypto/trisa/pkg/trisa/keys"
)

var (
	ErrNotAwaitingConfirmation = errors.New("transfer is not awaiting a confirmation")
)

// Confirmation applies an inbound TRP confirmation to the transfer it refers to: a
// transaction id completes the transfer, a cancellation rejects it.
func (s *Server) Confirmation(c *gin.Context) {
	var (
		err        error
		envelopeID uuid.UUID
		in         *trp.Confirmation
		status     enum.Status
		db         models.PreparedTransaction
	)

	if envelopeID, err = uuid.Parse(c.Param("envelopeID")); err != nil {
		c.AbortWithError(http.StatusNotFound, ErrUnknownTransfer)
		return
	}

	// The capability token is checked before anything touches the database so that an
	// unauthorized caller cannot use response timing to probe for envelope ids.
	if !s.authorizeCallback(c, envelopeID, callback.PurposeConfirm) {
		return
	}

	in = &trp.Confirmation{
		Info: TRPInfo(c),
	}

	if err = c.BindJSON(in); err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	if err = in.Validate(); err != nil {
		c.AbortWithError(http.StatusUnprocessableEntity, err)
		return
	}

	// A cancellation is a decision by the originator not to send the funds, which is
	// terminal in the same way a rejection is; there is no separate canceled status.
	status = enum.StatusCompleted

	if in.Canceled != "" {
		status = enum.StatusRejected
	}

	ctx := c.Request.Context()
	log := logger.Tracing(ctx).With().
		Str("envelope_id", envelopeID.String()).
		Str("status", status.String()).
		Logger()

	if db, err = s.store.PrepareTransaction(ctx, envelopeID, &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.Confirmation()"},
	}); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	// Rollback unless the confirmation is applied and committed below.
	defer db.Rollback()

	// PrepareTransaction creates a stub for unknown ids, which is rolled back here.
	if db.Created() {
		c.AbortWithError(http.StatusNotFound, ErrUnknownTransfer)
		return
	}

	var transaction *models.Transaction

	if transaction, err = db.Fetch(); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	log = log.With().
		Str("source", transaction.Source.String()).
		Str("transfer_status", transaction.Status.String()).
		Logger()

	// Only the originator of a transfer confirms it, so a transfer this node opened is
	// never confirmed by the remote. Answer 404 rather than revealing the envelope id.
	if transaction.Source != enum.SourceRemote {
		log.Warn().Msg("refusing trp confirmation for a transfer that did not originate with the counterparty")
		c.AbortWithError(http.StatusNotFound, ErrUnknownTransfer)

		return
	}

	var counterparty *models.Counterparty

	if transaction.CounterpartyID.Valid {
		if counterparty, err = s.store.RetrieveCounterparty(ctx, transaction.CounterpartyID.ULID); err != nil {
			log.Warn().Err(err).Msg("could not load the counterparty of a transfer receiving a trp confirmation")
		}
	}

	if counterparty == nil || counterparty.Protocol != enum.ProtocolTRP {
		log.Warn().Msg("refusing trp confirmation for a transfer that is not conducted over trp")
		c.AbortWithError(http.StatusNotFound, ErrUnknownTransfer)

		return
	}

	// Only an approved transfer can be confirmed: anything else has either not been
	// resolved yet or has already reached a terminal status.
	if transaction.Status != enum.StatusAccepted {
		log.Warn().Msg("refusing trp confirmation for a transfer that is not awaiting one")
		c.AbortWithError(http.StatusConflict, ErrNotAwaitingConfirmation)

		return
	}

	// Record the confirmation as an incoming secure envelope built on the payload of the
	// last message stored for this transfer, so the envelope history is complete.
	var packet *postman.TRPPacket

	if packet, err = s.confirmationPacket(ctx, envelopeID, in, c.Request.TLS); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not build the incoming envelope for a trp confirmation")
		c.AbortWithError(http.StatusInternalServerError, err)

		return
	}

	packet.Log = log
	packet.DB = db
	packet.Counterparty = counterparty

	var storageKey keys.PublicKey

	if storageKey, err = s.trisa.StorageKey("", counterparty.CommonName); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not get storage key for trp confirmation")
		c.AbortWithError(http.StatusInternalServerError, err)

		return
	}

	// Keep the cleartext envelope for the webhook: Seal replaces it with the sealed one.
	clear := packet.In.Envelope

	if err = packet.Seal(storageKey); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not seal trp confirmation")
		c.AbortWithError(http.StatusInternalServerError, err)

		return
	}

	notes := "Server.Confirmation()"

	if in.Canceled != "" {
		notes = fmt.Sprintf("Server.Confirmation(): canceled by the counterparty: %s", in.Canceled)
	}

	if err = db.AddEnvelope(packet.In.Model(), &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: notes},
	}); err != nil {
		log.Error().Err(err).Bool("stored_to_database", false).Msg("could not store incoming trp confirmation in database")
		c.AbortWithError(http.StatusInternalServerError, err)

		return
	}

	transaction.Status = status
	transaction.LastUpdate = sql.NullTime{Valid: true, Time: time.Now()}

	if err = db.Update(transaction, &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: notes},
	}); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	if err = db.Commit(); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	log.Info().Bool("stored_to_database", true).Msg("trp confirmation applied to transaction")

	// The confirmation is committed, so a callback that is down must not turn a settled
	// transfer into an error the originator would retry: notify and ignore the reply,
	// exactly as the TRISA server does when it echoes an error back.
	if s.WebhookEnabled() {
		packet.RevealIncoming(clear)

		request := packet.In.WebhookRequestFor(webhook.ProtocolTRP)

		if err = request.AddPayload(packet.Payload()); err != nil {
			log.Error().Err(err).Msg("could not add payload to webhook callback")
		} else if _, err = s.webhook.Callback(ctx, request); err != nil {
			log.Error().Err(err).Msg("could not execute webhook callback")
		}
	}

	// A 204 should be sent in response to a transfer confirmation.
	c.Status(http.StatusNoContent)
}

// confirmationPacket builds the incoming half of a confirmation from the payload of the
// most recent message stored for the transfer, which is where the identity record and
// the reference transaction come from.
func (s *Server) confirmationPacket(ctx context.Context, envelopeID uuid.UUID, in *trp.Confirmation, mtls *tls.ConnectionState) (packet *postman.TRPPacket, err error) {
	var base *trisa.Payload

	if base, err = s.latestPayload(ctx, envelopeID); err != nil {
		return nil, err
	}

	return postman.ReceiveTRPConfirmation(envelopeID, base, in, mtls)
}

// latestPayload decrypts the most recent stored envelope of a transfer and returns its
// payload. The TRP server has the same key material as the web server (both reach the
// keychain through the TRISA network), so it can read back the envelopes it sealed.
func (s *Server) latestPayload(ctx context.Context, envelopeID uuid.UUID) (payload *trisa.Payload, err error) {
	var model *models.SecureEnvelope

	if model, err = s.store.LatestPayloadEnvelope(ctx, envelopeID, enum.DirectionAny); err != nil {
		return nil, fmt.Errorf("could not retrieve the latest envelope of the transfer: %w", err)
	}

	var decrypted *envelope.Envelope

	if decrypted, err = s.decrypt(model); err != nil {
		return nil, fmt.Errorf("could not decrypt the latest envelope of the transfer: %w", err)
	}

	if payload, err = decrypted.Payload(); err != nil {
		return nil, fmt.Errorf("could not read the payload of the latest envelope: %w", err)
	}

	return payload, nil
}

// decrypt unseals a stored secure envelope with the node's own keys; it mirrors the web
// server's Decrypt, including the handling of outgoing envelopes whose keys are stored
// beside the envelope rather than in it.
func (s *Server) decrypt(in *models.SecureEnvelope) (out *envelope.Envelope, err error) {
	if in.IsError {
		return envelope.Wrap(in.Envelope)
	}

	if !in.PublicKey.Valid {
		return nil, errors.New("secure envelope is missing a public key signature")
	}

	var unsealingKey keys.PrivateKey

	if unsealingKey, err = s.trisa.UnsealingKey(in.PublicKey.String, in.Remote.String); err != nil {
		return nil, fmt.Errorf("could not lookup unsealing key for secure envelope: %w", err)
	}

	if in.Direction == enum.DirectionOutgoing {
		in.Envelope.EncryptionKey = in.EncryptionKey
		in.Envelope.HmacSecret = in.HMACSecret
	}

	if out, _, err = envelope.Open(in.Envelope, envelope.WithUnsealingKey(unsealingKey)); err != nil {
		return nil, err
	}

	return out, nil
}
