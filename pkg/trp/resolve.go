package trp

// Inbound TRP resolution callbacks.
//
// Upstream Envoy answers POST /transfers/:envelopeID/resolve with a bare 204 and
// discards the body, so an originating node never learns that the beneficiary VASP
// approved or rejected the transfer. This handler applies the resolution to the
// transaction the callback refers to.

import (
	"context"
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
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	"github.com/trisacrypto/trisa/pkg/trisa/envelope"
	"github.com/trisacrypto/trisa/pkg/trisa/keys"
)

var (
	ErrUnknownTransfer       = errors.New("no transfer with the specified envelope id")
	ErrNotAwaitingResolution = errors.New("transfer is not awaiting a resolution")
)

// Resolve applies an inbound TRP resolution to the referenced transaction: an
// approval marks it accepted, a rejection marks it rejected, and a version-only
// acknowledgement leaves it pending on the counterparty.
func (s *Server) Resolve(c *gin.Context) {
	var (
		err        error
		envelopeID uuid.UUID
		in         *trp.Resolution
		status     enum.Status
		db         models.PreparedTransaction
	)

	if envelopeID, err = uuid.Parse(c.Param("envelopeID")); err != nil {
		c.AbortWithError(http.StatusNotFound, ErrUnknownTransfer)
		return
	}

	// The capability token is checked before anything touches the database so that an
	// unauthorized caller cannot use response timing to probe for envelope ids.
	if !s.authorizeCallback(c, envelopeID, callback.PurposeResolve) {
		return
	}

	in = &trp.Resolution{
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

	switch {
	case in.Rejected != "":
		status = enum.StatusRejected
	case in.Approved != nil:
		status = enum.StatusAccepted
	default:
		status = enum.StatusPending
	}

	ctx := c.Request.Context()
	log := logger.Tracing(ctx).With().
		Str("envelope_id", envelopeID.String()).
		Str("status", status.String()).
		Logger()

	if db, err = s.store.PrepareTransaction(ctx, envelopeID, &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.Resolve()"},
	}); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	// Rollback unless the resolution is applied and committed below.
	defer db.Rollback()

	// A resolution can only update a transfer that this node already knows about;
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

	// This endpoint is unauthenticated in TRP-only deployments: without mTLS the
	// caller's identity cannot be verified, so anyone who can reach the node can post
	// a resolution for any envelope id it knows. The two checks below bound the damage
	// to transfers this node itself opened and is still waiting on, which stops a
	// remote sender from approving the inbound transfer it just submitted and skipping
	// local compliance review entirely.
	log = log.With().
		Str("source", transaction.Source.String()).
		Str("transfer_status", transaction.Status.String()).
		Logger()

	// Only an inquiry that originated here can be resolved by a counterparty; an
	// inbound transfer is resolved by the local compliance team instead. Answer 404 so
	// the refusal does not reveal that the envelope id exists.
	if transaction.Source != enum.SourceLocal {
		log.Warn().Msg("refusing trp resolution for a transfer that did not originate on this node")
		c.AbortWithError(http.StatusNotFound, ErrUnknownTransfer)

		return
	}

	// An outgoing inquiry sits in pending while it awaits the beneficiary's decision.
	// Any other status (accepted, rejected, review, completed, ...) has already been
	// resolved or is not resolvable, so a second resolution is refused.
	if transaction.Status != enum.StatusPending {
		log.Warn().Msg("refusing trp resolution for a transfer that is not awaiting one")
		c.AbortWithError(http.StatusConflict, ErrNotAwaitingResolution)

		return
	}

	// Pending TRISA and sunrise transfers are answered through their own protocols, so
	// a TRP callback may only touch a transfer whose counterparty is reached over TRP.
	var counterparty *models.Counterparty

	if transaction.CounterpartyID.Valid {
		if counterparty, err = s.store.RetrieveCounterparty(ctx, transaction.CounterpartyID.ULID); err != nil {
			log.Warn().Err(err).Msg("could not load the counterparty of a transfer receiving a trp resolution")
		}
	}

	if counterparty == nil || counterparty.Protocol != enum.ProtocolTRP {
		log.Warn().Msg("refusing trp resolution for a transfer that is not conducted over trp")
		c.AbortWithError(http.StatusNotFound, ErrUnknownTransfer)

		return
	}

	// Record the decision as an incoming secure envelope. Beyond the audit trail this is
	// load bearing for approvals: the confirmation this node sends when the transfer
	// completes has to go to the callback named in the approval, and this envelope is the
	// only place it is kept. A failure is therefore fatal: committing "accepted" without
	// the envelope would leave a transfer that can never be completed, and the
	// counterparty cannot resubmit once the status has moved. Answering 500 keeps the
	// transfer pending so the counterparty retries.
	if status != enum.StatusPending {
		if err = s.storeResolution(ctx, db, envelopeID, counterparty, in); err != nil {
			log.Error().Err(err).Bool("stored_to_database", false).Msg("could not store the incoming envelope for a trp resolution")
			c.AbortWithError(http.StatusInternalServerError, err)

			return
		}
	}

	transaction.Status = status
	transaction.LastUpdate = sql.NullTime{Valid: true, Time: time.Now()}

	if err = db.Update(transaction, &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.Resolve()"},
	}); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	if err = db.Commit(); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	log.Info().Msg("trp resolution applied to transaction")

	// A 204 should be sent in response to a transfer inquiry resolution.
	c.Status(http.StatusNoContent)
}

// storeResolution records an asynchronous resolution as an incoming secure envelope,
// built on the payload of the inquiry this node sent, and sealed with the node's own
// storage key because TRP messages are plaintext on the wire.
func (s *Server) storeResolution(ctx context.Context, db models.PreparedTransaction, envelopeID uuid.UUID, counterparty *models.Counterparty, in *trp.Resolution) (err error) {
	var base *trisa.Payload

	if base, err = s.latestPayload(ctx, envelopeID); err != nil {
		return err
	}

	var payload *trisa.Payload

	if payload, err = postman.PayloadFromResolution(base, in); err != nil {
		return err
	}

	transferState := trisa.TransferAccepted

	if in.Rejected != "" {
		transferState = trisa.TransferRejected
	}

	var env *envelope.Envelope

	opts := []envelope.Option{
		envelope.WithEnvelopeID(envelopeID.String()),
		envelope.WithTransferState(transferState),
	}

	if env, err = envelope.New(payload, opts...); err != nil {
		return fmt.Errorf("could not create incoming trp resolution envelope: %w", err)
	}

	var storageKey keys.PublicKey

	if storageKey, err = s.trisa.StorageKey("", counterparty.CommonName); err != nil {
		return fmt.Errorf("could not get storage key for trp resolution: %w", err)
	}

	if env, _, err = env.Encrypt(); err != nil {
		return fmt.Errorf("could not encrypt trp resolution: %w", err)
	}

	if env, _, err = env.Seal(envelope.WithSealingKey(storageKey)); err != nil {
		return fmt.Errorf("could not seal trp resolution: %w", err)
	}

	model := env.Proto()
	validHMAC, _ := env.ValidateHMAC()
	timestamp, _ := env.Timestamp()

	return db.AddEnvelope(&models.SecureEnvelope{
		EnvelopeID:    envelopeID,
		Direction:     enum.DirectionIncoming,
		Remote:        sql.NullString{Valid: counterparty.CommonName != "", String: counterparty.CommonName},
		IsError:       false,
		EncryptionKey: model.EncryptionKey,
		HMACSecret:    model.HmacSecret,
		ValidHMAC:     sql.NullBool{Valid: true, Bool: validHMAC},
		PublicKey:     sql.NullString{Valid: model.PublicKeySignature != "", String: model.PublicKeySignature},
		TransferState: int32(model.TransferState),
		Timestamp:     timestamp,
		Envelope:      model,
	}, &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.Resolve()"},
	})
}
