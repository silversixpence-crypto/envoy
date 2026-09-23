package web

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
	api "github.com/trisacrypto/envoy/pkg/web/api/v1"

	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
)

// sendTimeout bounds a send once it has started. It is longer than the 30 second
// timeout of the TRP client (pkg/trp/client) so that it never cuts off a counterparty
// that is still answering within that timeout.
const sendTimeout = 60 * time.Second

// Send performs the bulk of the work to send a travel rule transfer to the
// counterparty specified and storing both the outgoing and incoming secure envelopes in
// the database. This method is used to send the prepared transaction, to send envelopes
// for a transaction, and in the accept/reject workflows.
func (s *Server) Send(c *gin.Context, routing *api.Routing, payload *trisa.Payload) (packet *postman.Packet, err error) {
	return s.SendWithID(c, uuid.New(), false, routing, payload)
}

// SendWithID is Send with an envelope id chosen by the caller. If requireNew is true
// and a transaction with that id already exists, nothing is created or sent: the
// existing transaction is returned on the packet with ErrEnvelopeExists and, unlike
// every other error, no response is written so that the caller can decide how to
// answer a repeated request.
//
// The database transaction and the send to the counterparty run on a context that is
// detached from the request, so a caller that gives up waiting and disconnects does
// not abort a send in flight: it still commits under the caller's id, and a retry with
// that id finds it. If the node itself fails the send (the counterparty is unreachable,
// or its response is lost or unusable) the transaction is rolled back as usual, so a
// retry after such a failure sends again: the node cannot know whether the counterparty
// saw the first attempt.
func (s *Server) SendWithID(c *gin.Context, envelopeID uuid.UUID, requireNew bool, routing *api.Routing, payload *trisa.Payload) (packet *postman.Packet, err error) {
	// Create a packet to begin the sending process
	if packet, err = postman.Send(envelopeID, payload, trisa.TransferStarted); err != nil {
		c.Error(err)
		c.JSON(http.StatusInternalServerError, api.Error("could not process send prepared transaction request"))
		return nil, err
	}

	// Detach from the request so that a disconnecting caller cannot cancel the send or
	// roll back the database transaction (database/sql rolls a transaction back when
	// its context is done); request values such as the audit actor are kept.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), sendTimeout)
	defer cancel()

	// Add the log to the packet for debugging
	packet.Log = logger.Tracing(ctx).With().Str("envelope_id", envelopeID.String()).Logger()

	// Lookup the counterparty from the travel address in the request
	if packet.Counterparty, err = s.ResolveCounterparty(c, routing); err != nil {
		// NOTE: CounterpartyFromTravelAddress handles API response back to user.
		return nil, err
	}

	// Create the transaction in the database
	if packet.DB, err = s.store.PrepareTransaction(ctx, envelopeID, &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.Send()"},
	}); err != nil {
		c.Error(err)
		c.JSON(http.StatusInternalServerError, api.Error("could not process send prepared transaction request"))
		return nil, err
	}
	defer packet.DB.Rollback()

	// The transaction already exists, most likely because the caller is repeating a
	// request that timed out while the first one was still waiting on the counterparty.
	if requireNew && !packet.DB.Created() {
		if packet.Transaction, err = packet.DB.Fetch(); err != nil {
			c.Error(err)
			c.JSON(http.StatusInternalServerError, api.Error("could not process send prepared transaction request"))
			return nil, err
		}

		return packet, ErrEnvelopeExists
	}

	// Add the counterparty to the database associated with the transaction
	// If the update fails, log the error but do not cancel processing.
	if err = packet.Out.UpdateTransaction(); err != nil {
		c.Error(err)
	}

	// The protocol was already parsed in ResolveCounterparty
	protocol, _ := enum.ParseProtocol(routing.Protocol)
	if packet, err = s.SendPacket(ctx, protocol, packet); err != nil {
		c.Error(err)
		if errors.Is(err, ErrUnavailable) {
			c.JSON(http.StatusBadGateway, api.Error(err))
			return nil, err
		} else if errors.Is(err, ErrDisabled) {
			c.JSON(http.StatusFailedDependency, api.Error(err))
			return nil, err
		}

		c.JSON(http.StatusInternalServerError, api.Error("could not send transfer message to counterparty"))
		return nil, err
	}

	// Update transaction state based on response from counterparty
	// If the update fails rollback and return the error.
	if err = packet.In.UpdateTransaction(); err != nil {
		c.Error(err)
		c.JSON(http.StatusInternalServerError, api.Error("could not process send prepared transaction request"))
		return nil, err
	}

	// Read the record from the database to return to the user
	if err = packet.RefreshTransaction(); err != nil {
		c.Error(err)
		c.JSON(http.StatusInternalServerError, api.Error("could not process send prepared transaction request"))
		return nil, err
	}

	// Commit the transaction to the database
	if err = packet.DB.Commit(); err != nil {
		c.Error(err)
		c.JSON(http.StatusInternalServerError, api.Error("could not process send prepared transaction request"))
		return nil, err
	}

	return packet, nil
}

// repeatedSend reports whether an existing transaction is the result of an earlier
// send of the same payload to the same counterparty from this node, so that a request
// that reuses its envelope id can be answered with it instead of being refused.
//
// It compares the counterparty and every summary field the transaction stores from
// the payload (see postman.TransactionFromPayload): the originator and beneficiary
// names and addresses, the virtual asset (network and asset type) and the amount. The
// rest of the IVMS101 identity is not compared: it is stored only inside the encrypted
// envelopes, and decrypting them to answer a retry is not worth the cost.
func repeatedSend(existing *models.Transaction, counterparty *models.Counterparty, payload *trisa.Payload) bool {
	if existing == nil || counterparty == nil || payload == nil || existing.Source != enum.SourceLocal {
		return false
	}

	if !existing.CounterpartyID.Valid || existing.CounterpartyID.ULID != counterparty.ID {
		return false
	}

	requested := postman.TransactionFromPayload(payload)

	// PrepareTransaction stores a placeholder when the payload names no asset.
	if requested.VirtualAsset == "" {
		requested.VirtualAsset = models.VirtualAssetUnknown
	}

	return existing.Originator.String == requested.Originator.String &&
		existing.OriginatorAddress.String == requested.OriginatorAddress.String &&
		existing.Beneficiary.String == requested.Beneficiary.String &&
		existing.BeneficiaryAddress.String == requested.BeneficiaryAddress.String &&
		existing.VirtualAsset == requested.VirtualAsset &&
		existing.Amount == requested.Amount
}

func (s *Server) SendPacket(ctx context.Context, protocol enum.Protocol, packet *postman.Packet) (_ *postman.Packet, err error) {
	// Step 1: Determine the protocol and use the correct handler to send the outgoing
	// packet (which might be updated during the send process) and to receive the
	// incoming reply from the counterparty.
	switch protocol {
	case enum.ProtocolTRISA:
		if !s.conf.Node.Enabled {
			return nil, ErrDisabled
		}

		wrapped := packet.TRISA()
		if err = s.SendTRISA(ctx, wrapped); err != nil {
			return nil, err
		}
		packet = &wrapped.Packet
	case enum.ProtocolTRP:
		if !s.conf.TRP.Enabled {
			return nil, ErrDisabled
		}

		// TRP messages are plaintext on the wire, so SendTRP seals and stores both
		// envelopes with the local storage key and we return early.
		wrapped := packet.TRP()

		if err = s.SendTRP(ctx, wrapped); err != nil {
			return nil, err
		}

		return &wrapped.Packet, nil
	case enum.ProtocolSunrise:
		if !s.conf.Sunrise.Enabled {
			return nil, ErrDisabled
		}

		wrapped := packet.Sunrise()
		if err = s.SendSunrise(ctx, wrapped); err != nil {
			return nil, err
		}
		packet = &wrapped.Packet
	default:
		return nil, fmt.Errorf("unhandled protocol in send packet: %q", protocol.String())
	}

	// TODO: right now sunrise has a special envelope storage method, so we exit early
	// but we should unify this with the TRISA and TRP methods.
	if protocol == enum.ProtocolSunrise {
		return packet, nil
	}

	// Step 2: Store the outgoing envelope by fetching the public key used to seal the
	// incoming envelope from key storage. and saving to the database.
	if packet.Out.StorageKey, err = s.trisa.StorageKey(packet.In.PublicKeySignature(), packet.Counterparty.CommonName); err != nil {
		// TODO: use the default keys if the incoming key is not known
		return nil, fmt.Errorf("could not fetch storage key: %w", err)
	}

	if err = packet.DB.AddEnvelope(packet.Out.Model(), &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.SendPacket()"},
	}); err != nil {
		return nil, fmt.Errorf("could not store outgoing envelope: %w", err)
	}

	// Step 3: Save incoming envelope to the database (should be encrypted with keys we
	// sent during the key exchange process of the transfer).
	if err = packet.DB.AddEnvelope(packet.In.Model(), &models.ComplianceAuditLog{
		ChangeNotes: sql.NullString{Valid: true, String: "Server.SendPacket()"},
	}); err != nil {
		return nil, fmt.Errorf("could not store incoming message: %w", err)
	}

	return packet, nil
}
