package trp_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/trisacrypto/envoy/pkg/enum"
	storemock "github.com/trisacrypto/envoy/pkg/store/mock"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/envoy/pkg/trisa/network"
	"github.com/trisacrypto/envoy/pkg/trp/callback"
	"github.com/trisacrypto/envoy/pkg/webhook"
	"github.com/trisacrypto/trisa/pkg/ivms101"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	generic "github.com/trisacrypto/trisa/pkg/trisa/data/generic/v1beta1"
	"github.com/trisacrypto/trisa/pkg/trisa/envelope"
	"go.rtnl.ai/ulid"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	testCounterpartyCN = "beneficiary.example.com"
	testPaymentAddress = "mvE3W7Z3V5wRCXFcJgTa6BQhcEjJ5sUJPP"
)

// resolveBody is the approval a counterparty posts back for an outbound inquiry.
func resolveBody() map[string]any {
	return map[string]any{
		"approved": map[string]string{
			"address":  testPaymentAddress,
			"callback": "https://" + testCounterpartyCN + "/confirm",
		},
	}
}

// resolvePath is the tokenized resolution callback for the transfer under test.
func resolvePath(t *testing.T) string {
	t.Helper()

	return "/transfers/" + testEnvelopeID + "/resolve/" + callback.Token(mustKey(t), testEnvelopeID, callback.PurposeResolve)
}

// storedInquiry builds the secure envelope of the inquiry this node is supposed to have
// sent. The resolution handler reads it back to recover the identity record and the
// reference transaction, so it has to be sealed with the same keychain the node uses.
func storedInquiry(t *testing.T, net network.Network) *models.SecureEnvelope {
	t.Helper()

	identity, err := anypb.New(&ivms101.IdentityPayload{})
	require.NoError(t, err, "could not marshal the identity payload")

	transaction, err := anypb.New(&generic.TRP{
		EnvelopeId: testEnvelopeID,
		Headers: &generic.TRPInfo{
			Version:           "3.1.0",
			RequestIdentifier: testEnvelopeID,
		},
		Message: &generic.TRP_Inquiry{
			Inquiry: &generic.TRPInquiry{
				Asset:    map[string]string{"slip044": "BTC"},
				Amount:   0.25,
				Callback: "https://origin.example.com/transfers/" + testEnvelopeID,
			},
		},
		Transaction: &generic.Transaction{
			Originator:  "Alice Originator",
			Beneficiary: "Bob Beneficiary",
			Amount:      0.25,
			Network:     "BTC",
		},
	})
	require.NoError(t, err, "could not marshal the trp payload")

	env, err := envelope.New(&trisa.Payload{
		Identity:    identity,
		Transaction: transaction,
		SentAt:      time.Now().UTC().Format(time.RFC3339),
	},
		envelope.WithEnvelopeID(testEnvelopeID),
		envelope.WithTransferState(trisa.TransferStarted),
	)
	require.NoError(t, err, "could not create the stored envelope")

	env, _, err = env.Encrypt()
	require.NoError(t, err, "could not encrypt the stored envelope")

	storageKey, err := net.StorageKey("", testCounterpartyCN)
	require.NoError(t, err, "could not get the node storage key")

	env, _, err = env.Seal(envelope.WithSealingKey(storageKey))
	require.NoError(t, err, "could not seal the stored envelope")

	model := env.Proto()

	return &models.SecureEnvelope{
		EnvelopeID:    uuid.MustParse(testEnvelopeID),
		Direction:     enum.DirectionIncoming,
		Remote:        sql.NullString{Valid: true, String: testCounterpartyCN},
		EncryptionKey: model.EncryptionKey,
		HMACSecret:    model.HmacSecret,
		PublicKey:     sql.NullString{Valid: true, String: model.PublicKeySignature},
		TransferState: int32(model.TransferState),
		Envelope:      model,
	}
}

// resolveFixture is a node whose only transfer is an outbound TRP inquiry that is still
// awaiting the beneficiary's decision, with the database writes captured so that a test
// can see what the handler committed.
type resolveFixture struct {
	srv         *httptest.Server
	store       *storemock.Store
	db          *storemock.PreparedTransaction
	transaction *models.Transaction
	envelopes   []*models.SecureEnvelope
}

// newResolveFixture wires that node up against the given callback handler; a nil handler
// is a node with no webhook configured.
func newResolveFixture(t *testing.T, hook webhook.Handler) *resolveFixture {
	t.Helper()

	srv, mockStore, net := newTestNode(t, false, hook)

	f := &resolveFixture{
		srv:   srv,
		store: mockStore,
		transaction: &models.Transaction{
			Source:         enum.SourceLocal,
			Status:         enum.StatusPending,
			CounterpartyID: ulid.NullULID{Valid: true, ULID: ulid.MakeSecure()},
		},
	}

	f.db = prepared(t, mockStore, f.transaction)

	f.db.OnUpdate(func(in *models.Transaction, _ *models.ComplianceAuditLog) error {
		f.transaction = in
		return nil
	})

	f.db.OnAddEnvelope(func(in *models.SecureEnvelope, _ *models.ComplianceAuditLog) error {
		f.envelopes = append(f.envelopes, in)
		return nil
	})

	mockStore.OnRetrieveCounterparty = func(context.Context, ulid.ULID) (*models.Counterparty, error) {
		return &models.Counterparty{
			Protocol:   enum.ProtocolTRP,
			CommonName: testCounterpartyCN,
			Endpoint:   "https://" + testCounterpartyCN,
			Name:       "Beneficiary VASP",
		}, nil
	}

	stored := storedInquiry(t, net)

	mockStore.OnLatestPayloadEnvelope = func(context.Context, uuid.UUID, enum.Direction) (*models.SecureEnvelope, error) {
		return stored, nil
	}

	return f
}

// The notification is delivered off the request goroutine, so a test cannot read a
// counter the moment the 204 arrives. These wrap the mock so every case signals, and a
// test waits for the signal (or, for the cases that must not notify, waits for silence).
func signalling(reply *webhook.Reply, fail error) (*webhook.Mock, <-chan *webhook.Request) {
	seen := make(chan *webhook.Request, 4)
	hook := webhook.NewMock()

	hook.OnCallback = func(_ context.Context, req *webhook.Request) (*webhook.Reply, error) {
		seen <- req

		if fail != nil {
			return nil, fail
		}

		if reply != nil {
			return reply, nil
		}

		return &webhook.Reply{TransactionID: req.TransactionID}, nil
	}

	return hook, seen
}

func awaitCallback(t *testing.T, seen <-chan *webhook.Request) *webhook.Request {
	t.Helper()

	select {
	case req := <-seen:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("expected exactly one webhook callback")

		return nil
	}
}

// Silence has to be waited for, not sampled: the callback is asynchronous, so an
// immediate check would pass even if one were on its way.
func requireNoCallback(t *testing.T, seen <-chan *webhook.Request) {
	t.Helper()

	select {
	case <-seen:
		t.Fatal("did not expect a webhook callback")
	case <-time.After(250 * time.Millisecond):
	}
}

// TestResolutionWebhook covers the compliance callback fired when a counterparty
// resolves an inquiry this node sent: without it the node knows the outcome of the
// operator's own outbound transfer and the operator does not.
func TestResolutionWebhook(t *testing.T) {
	t.Run("Approved", func(t *testing.T) {
		hook, seen := signalling(nil, nil)

		f := newResolveFixture(t, hook)

		rep := post(t, f.srv, resolvePath(t), resolveBody())
		require.Equal(t, http.StatusNoContent, rep.StatusCode)

		request := awaitCallback(t, seen)
		require.NotNil(t, request, "the webhook was called without a request")

		// The transfer is identified by its envelope id and the outcome is readable
		// both from the transfer state and from the TRP message in the payload.
		require.Equal(t, testEnvelopeID, request.TransactionID.String())
		require.Equal(t, webhook.ProtocolTRP, request.Protocol)
		require.Equal(t, trisa.TransferAccepted.String(), request.TransferState)
		require.NotNil(t, request.Payload, "the webhook request had no payload")
		require.Equal(t, testPaymentAddress, request.Payload.TRP.GetApproved().GetAddress())
		require.Equal(t, "Bob Beneficiary", request.Payload.Transaction.GetBeneficiary())

		require.Equal(t, enum.StatusAccepted, f.transaction.Status)
		require.Len(t, f.envelopes, 1, "expected the resolution to be stored as one envelope")
		f.db.AssertCommit(t)
	})

	t.Run("Rejected", func(t *testing.T) {
		hook, seen := signalling(nil, nil)

		f := newResolveFixture(t, hook)

		rep := post(t, f.srv, resolvePath(t), map[string]string{"rejected": "not our customer"})
		require.Equal(t, http.StatusNoContent, rep.StatusCode)

		request := awaitCallback(t, seen)
		require.Equal(t, testEnvelopeID, request.TransactionID.String())
		require.Equal(t, trisa.TransferRejected.String(), request.TransferState)
		require.Equal(t, "not our customer", request.Payload.TRP.GetRejected().GetRejected())

		require.Equal(t, enum.StatusRejected, f.transaction.Status)
		f.db.AssertCommit(t)
	})

	t.Run("Acknowledgement", func(t *testing.T) {
		// A version-only resolution is "received, decision to follow"; there is no
		// outcome to report and nothing is stored, so no callback is made.
		hook, seen := signalling(nil, nil)

		f := newResolveFixture(t, hook)

		rep := post(t, f.srv, resolvePath(t), map[string]string{"version": "3.1.0"})
		require.Equal(t, http.StatusNoContent, rep.StatusCode)

		requireNoCallback(t, seen)
		require.Equal(t, enum.StatusPending, f.transaction.Status)
		require.Empty(t, f.envelopes)
	})

	t.Run("CallbackDown", func(t *testing.T) {
		// The counterparty is owed its 204 whether or not the operator's callback is
		// reachable, and the resolution has already been committed by then.
		hook, seen := signalling(nil, errors.New("connection refused"))

		f := newResolveFixture(t, hook)

		rep := post(t, f.srv, resolvePath(t), resolveBody())
		require.Equal(t, http.StatusNoContent, rep.StatusCode)

		awaitCallback(t, seen)
		require.Equal(t, enum.StatusAccepted, f.transaction.Status, "a webhook failure must not undo the counterparty's decision")
		require.Len(t, f.envelopes, 1)
		f.db.AssertCommit(t)
	})

	t.Run("NoWebhook", func(t *testing.T) {
		// A node with no callback configured resolves exactly as it did before.
		f := newResolveFixture(t, nil)

		rep := post(t, f.srv, resolvePath(t), resolveBody())
		require.Equal(t, http.StatusNoContent, rep.StatusCode)

		require.Equal(t, enum.StatusAccepted, f.transaction.Status)
		require.Len(t, f.envelopes, 1)
		f.db.AssertCommit(t)
		f.db.AssertCalls(t, "Update", 1)
	})

	t.Run("ReplyCannotChangeStatus", func(t *testing.T) {
		// The counterparty has already decided, so the callback is a notification: a
		// reply that asks for a different transfer action, or returns an error, is
		// answered after the commit and has no path back to the stored status.
		testCases := []struct {
			name  string
			reply *webhook.Reply
		}{
			{"rejects the approval", &webhook.Reply{TransferAction: "rejected"}},
			{"asks for a repair", &webhook.Reply{TransferAction: "repair"}},
			{"asks for review", &webhook.Reply{TransferAction: "review"}},
			{"returns an error", &webhook.Reply{Error: &trisa.Error{Code: trisa.ComplianceCheckFail, Message: "no"}}},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				hook, seen := signalling(tc.reply, nil)

				f := newResolveFixture(t, hook)

				rep := post(t, f.srv, resolvePath(t), resolveBody())
				require.Equal(t, http.StatusNoContent, rep.StatusCode)

				awaitCallback(t, seen)
				require.Equal(t, enum.StatusAccepted, f.transaction.Status, "the webhook reply changed the stored status")
				f.db.AssertCalls(t, "Update", 1)
				f.db.AssertCommit(t)
			})
		}
	})
}
