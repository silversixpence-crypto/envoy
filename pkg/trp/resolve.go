package trp

// Inbound TRP resolution callbacks.
//
// Upstream Envoy answers POST /transfers/:envelopeID/resolve with a bare 204 and
// discards the body, so an originating node never learns that the beneficiary VASP
// approved or rejected the transfer. This handler applies the resolution to the
// transaction the callback refers to.

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/trisacrypto/envoy/pkg/enum"
	"github.com/trisacrypto/envoy/pkg/logger"
	"github.com/trisacrypto/envoy/pkg/postman"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/envoy/pkg/trp/callback"
	"github.com/trisacrypto/envoy/pkg/webhook"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
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

	// The callback request is built inside the transaction but posted after the commit,
	// so it is declared out here. It stays nil for a version-only acknowledgement, which
	// records nothing and is not an outcome to notify the operator about, and for a node
	// with no webhook configured.
	var hookReq *webhook.Request

	// Record the decision as an incoming secure envelope. Beyond the audit trail this is
	// load bearing for approvals: the confirmation this node sends when the transfer
	// completes has to go to the callback named in the approval, and this envelope is the
	// only place it is kept. A failure is therefore fatal: committing "accepted" without
	// the envelope would leave a transfer that can never be completed, and the
	// counterparty cannot resubmit once the status has moved. Answering 500 keeps the
	// transfer pending so the counterparty retries.
	if status != enum.StatusPending {
		var packet *postman.TRPPacket

		if packet, err = s.resolutionPacket(ctx, envelopeID, in, c.Request.TLS); err != nil {
			log.Error().Err(err).Bool("stored_to_database", false).Msg("could not build the incoming envelope for a trp resolution")
			c.AbortWithError(http.StatusInternalServerError, err)

			return
		}

		packet.Log = log
		packet.DB = db
		packet.Counterparty = counterparty

		var storageKey keys.PublicKey

		if storageKey, err = s.trisa.StorageKey("", counterparty.CommonName); err != nil {
			log.Error().Err(err).Bool("stored_to_database", false).Msg("could not get storage key for trp resolution")
			c.AbortWithError(http.StatusInternalServerError, err)

			return
		}

		// Keep the cleartext envelope for the webhook: Seal replaces it with the sealed one.
		clear := packet.In.Envelope

		if err = packet.Seal(storageKey); err != nil {
			log.Error().Err(err).Bool("stored_to_database", false).Msg("could not seal trp resolution")
			c.AbortWithError(http.StatusInternalServerError, err)

			return
		}

		if err = db.AddEnvelope(packet.In.Model(), &models.ComplianceAuditLog{
			ChangeNotes: sql.NullString{Valid: true, String: "Server.Resolve()"},
		}); err != nil {
			log.Error().Err(err).Bool("stored_to_database", false).Msg("could not store the incoming envelope for a trp resolution")
			c.AbortWithError(http.StatusInternalServerError, err)

			return
		}

		// Build the callback request here, where the packet still exists, so that the
		// notification below is handed nothing but the request: see notifyResolution for
		// why it must not be able to reach the transfer. A payload that cannot be
		// assembled costs the operator a notification but must not cost the counterparty
		// its 204, so it is logged and the resolution continues without one.
		if s.WebhookEnabled() {
			packet.RevealIncoming(clear)

			hookReq = packet.In.WebhookRequestFor(webhook.ProtocolTRP)

			// The transfer state on the request is accepted or rejected and the TRP
			// message in the payload carries the approval address or the rejection
			// comment, so a receiver can tell the two outcomes apart and identify the
			// transfer by its envelope id.
			if err = hookReq.AddPayload(packet.Payload()); err != nil {
				log.Error().Err(err).Msg("could not add payload to webhook callback")
				hookReq = nil
			}
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

	// Notify the operator's back office that their outbound transfer was decided. This
	// deliberately sits after the commit: a callback may block for the full 30 second
	// webhook timeout, and on SQLite a write transaction held that long stalls every
	// other writer on the node. It also means the status the counterparty set is already
	// durable before anything outside the node is told about it.
	if hookReq != nil {
		s.notifyResolution(context.WithoutCancel(ctx), hookReq, log)
	}

	// A 204 should be sent in response to a transfer inquiry resolution.
	c.Status(http.StatusNoContent)
}

// resolutionPacket builds the incoming half of an asynchronous resolution from the
// payload of the inquiry this node sent, which is where the identity record and the
// reference transaction come from. The envelope is sealed with the node's own storage
// key by the caller, because TRP messages are plaintext on the wire.
func (s *Server) resolutionPacket(ctx context.Context, envelopeID uuid.UUID, in *trp.Resolution, mtls *tls.ConnectionState) (packet *postman.TRPPacket, err error) {
	var base *trisa.Payload

	if base, err = s.latestPayload(ctx, envelopeID); err != nil {
		return nil, err
	}

	return postman.ReceiveTRPResolution(envelopeID, base, in, mtls)
}

// notifyResolution posts a resolution to the compliance callback so that the operator
// who sent the inquiry learns the counterparty's decision; without it the node knows the
// transfer was accepted or rejected and the back office that originated it does not.
//
// This is a notification, not a decision, which is the difference between it and
// WebhookInquiry. On an inquiry the reply is what this node answers the counterparty
// with; here the counterparty has already decided and the decision is committed before
// this is called. The signature is what keeps it that way: the only arguments are the
// request to post and a logger, so the packet, the prepared transaction and the
// transaction model are all out of reach, there is no return value to carry a verdict
// back to the handler, and the *webhook.Reply is dropped. A callback therefore has no
// path by which it could move the stored status.
//
// Errors are logged rather than returned for the same reason the TRISA server swallows
// them: the counterparty is owed its 204 whether or not the operator's callback is
// reachable, and the resolution is already durable.
func (s *Server) notifyResolution(ctx context.Context, request *webhook.Request, log zerolog.Logger) {
	// Delivered off the request goroutine, and on a context detached from it.
	//
	// The webhook client allows 30 seconds and this server's WriteTimeout is 20, so a
	// slow back office would otherwise cost the counterparty its 204 on a resolution
	// this node has already committed. The counterparty's send then fails, and its
	// retry gets a 409 because the transfer is no longer pending: the two nodes end up
	// disagreeing about a decision that was in fact recorded. The counterparty's
	// acknowledgement must not depend on how fast our operator's endpoint answers.
	//
	// The cost of that choice is that a notification in flight when the process stops
	// is lost. Making it durable means an outbox in the node, which is a larger change
	// than this one; until then a missed callback is recoverable from the envelope
	// trail, while an unacknowledged resolution is not.
	go func() {
		if _, err := s.webhook.Callback(ctx, request); err != nil {
			log.Error().Err(err).Msg("could not execute webhook callback")
		}
	}()
}
