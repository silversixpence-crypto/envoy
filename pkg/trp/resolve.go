package trp

// Inbound TRP resolution callbacks.
//
// Upstream Envoy answers POST /transfers/:envelopeID/resolve with a bare 204 and
// discards the body, so an originating node never learns that the beneficiary VASP
// approved or rejected the transfer. This handler applies the resolution to the
// transaction the callback refers to.

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/trisacrypto/envoy/pkg/enum"
	"github.com/trisacrypto/envoy/pkg/logger"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
)

var ErrUnknownTransfer = errors.New("no transfer with the specified envelope id")

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
