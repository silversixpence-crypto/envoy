package trp_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/trisacrypto/envoy/pkg/bufconn"
	"github.com/trisacrypto/envoy/pkg/config"
	"github.com/trisacrypto/envoy/pkg/enum"
	"github.com/trisacrypto/envoy/pkg/store"
	storemock "github.com/trisacrypto/envoy/pkg/store/mock"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/envoy/pkg/trisa/network"
	"github.com/trisacrypto/envoy/pkg/trp"
	"github.com/trisacrypto/envoy/pkg/trp/callback"
	"github.com/trisacrypto/envoy/pkg/webhook"
	"github.com/trisacrypto/trisa/pkg/openvasp"

	"github.com/google/uuid"
	"go.rtnl.ai/ulid"
)

// mustKey decodes the test callback key that the test server is configured with.
func mustKey(t *testing.T) []byte {
	t.Helper()

	key, err := hex.DecodeString(testCallbackKey)
	require.NoError(t, err, "could not decode the test callback key")

	return key
}

const (
	testEnvelopeID  = "5f1a6bcb-4e2b-4a9d-bc9f-3e9a2d3f5c11"
	testCallbackKey = "b1f8ad0a9a1c32ec19a24b0cbc1e5b0fdc5dd3ad4ee62b9b09b8b3b8fdd4a9c7"
)

// newTestServer starts a TRP server backed by a mock store and no compliance callback;
// the returned store is the one the handlers use so that a test can assert which calls
// reached the database.
func newTestServer(t *testing.T, allowUnauthenticated bool) (*httptest.Server, *storemock.Store) {
	t.Helper()

	srv, mockStore, _ := newTestNode(t, allowUnauthenticated, nil)

	return srv, mockStore
}

// newTestNode is newTestServer with the compliance callback handler under the test's
// control; a nil hook is a node with no webhook configured. The mocked TRISA network is
// returned as well because its keychain is what seals the stored envelopes the handlers
// read back, so a test that needs a transfer history has to use the same keys.
func newTestNode(t *testing.T, allowUnauthenticated bool, hook webhook.Handler) (*httptest.Server, *storemock.Store, network.Network) {
	t.Helper()

	conf := config.Config{
		TRP: config.TRPConfig{
			Enabled:                       true,
			BindAddr:                      "127.0.0.1:0",
			Endpoint:                      "trp.example.com",
			UseMTLS:                       false,
			CallbackKey:                   testCallbackKey,
			AllowUnauthenticatedCallbacks: allowUnauthenticated,
		},
	}

	db, err := store.Open("mock:///")
	require.NoError(t, err, "could not open the mock store")

	mockStore, ok := db.(*storemock.Store)
	require.True(t, ok, "expected a mock store")

	// The mocked network loads its certificates relative to the working directory, which
	// is this package when the tests run.
	net, err := network.NewMocked(&config.TRISAConfig{
		MTLSConfig: config.MTLSConfig{
			Pool:  "../trisa/network/testdata/pool.pem",
			Certs: "../trisa/network/testdata/alice.pem",
		},
		KeyExchangeCacheTTL: time.Second,
		Directory: config.DirectoryConfig{
			Insecure:        true,
			Endpoint:        bufconn.Endpoint,
			MembersEndpoint: bufconn.Endpoint,
		},
	})
	require.NoError(t, err, "could not create a mocked trisa network")

	inner := &http.Server{}
	_, err = trp.Debug(conf, db, net, hook, inner)
	require.NoError(t, err, "could not create the trp server")

	srv := httptest.NewServer(inner.Handler)
	t.Cleanup(srv.Close)

	return srv, mockStore, net
}

// post sends a TRP request with the headers the core protocol requires.
func post(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
	t.Helper()

	data, err := json.Marshal(body)
	require.NoError(t, err, "could not marshal the request body")

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(data))
	require.NoError(t, err, "could not create the request")

	req.Header.Set(openvasp.APIVersionHeader, "3.2.1")
	req.Header.Set(openvasp.RequestIdentifierHeader, testEnvelopeID)
	req.Header.Set(openvasp.ContentTypeHeader, "application/json")

	rep, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "could not execute the request")

	t.Cleanup(func() { rep.Body.Close() })

	return rep
}

// prepared returns a mock prepared transaction for a transfer in the given state.
func prepared(t *testing.T, mockStore *storemock.Store, transaction *models.Transaction) *storemock.PreparedTransaction {
	t.Helper()

	db := &storemock.PreparedTransaction{}
	db.Reset()
	db.OnCreated(func() bool { return transaction == nil })
	db.OnFetch(func() (*models.Transaction, error) { return transaction, nil })
	db.OnRollback(func() error { return nil })

	mockStore.OnPrepareTransaction = func(_ context.Context, _ uuid.UUID, _ *models.ComplianceAuditLog) (models.PreparedTransaction, error) {
		return db, nil
	}

	return db
}

// TestCallbackTokens covers the capability token gate on the resolve and confirm
// callbacks: without a valid token nothing about the transfer is disclosed, and the
// database is never touched.
func TestCallbackTokens(t *testing.T) {
	resolveToken := callback.Token(mustKey(t), testEnvelopeID, callback.PurposeResolve)
	confirmToken := callback.Token(mustKey(t), testEnvelopeID, callback.PurposeConfirm)

	t.Run("Refused", func(t *testing.T) {
		testCases := []struct {
			name string
			path string
			body any
		}{
			{"tokenless resolve", "/transfers/" + testEnvelopeID + "/resolve", map[string]string{"rejected": "no"}},
			{"tokenless confirm", "/transfers/" + testEnvelopeID + "/confirm", map[string]string{"txid": "0x1"}},
			{"wrong resolve token", "/transfers/" + testEnvelopeID + "/resolve/deadbeef", map[string]string{"rejected": "no"}},
			{"confirm token on resolve", "/transfers/" + testEnvelopeID + "/resolve/" + confirmToken, map[string]string{"rejected": "no"}},
			{"resolve token on confirm", "/transfers/" + testEnvelopeID + "/confirm/" + resolveToken, map[string]string{"txid": "0x1"}},
			{"unparseable transfer id", "/transfers/not-a-uuid/resolve/" + resolveToken, map[string]string{"rejected": "no"}},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				srv, mockStore := newTestServer(t, false)

				rep := post(t, srv, tc.path, tc.body)
				require.Equal(t, http.StatusNotFound, rep.StatusCode)

				mockStore.AssertCalls(t, "PrepareTransaction", 0)
			})
		}
	})

	t.Run("Accepted", func(t *testing.T) {
		// A valid token gets as far as the database, where an unknown transfer is
		// refused with the same 404 an unauthorized caller sees.
		testCases := []struct {
			name string
			path string
			body any
		}{
			{"resolve", "/transfers/" + testEnvelopeID + "/resolve/" + resolveToken, map[string]string{"rejected": "no"}},
			{"confirm", "/transfers/" + testEnvelopeID + "/confirm/" + confirmToken, map[string]string{"txid": "0x1"}},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				srv, mockStore := newTestServer(t, false)
				prepared(t, mockStore, nil)

				rep := post(t, srv, tc.path, tc.body)
				require.Equal(t, http.StatusNotFound, rep.StatusCode)

				mockStore.AssertCalls(t, "PrepareTransaction", 1)
			})
		}
	})

	t.Run("UnauthenticatedAllowed", func(t *testing.T) {
		// The legacy tokenless routes stay usable for nodes that opt in, so that
		// callbacks issued before tokens existed can still be answered.
		srv, mockStore := newTestServer(t, true)
		prepared(t, mockStore, nil)

		rep := post(t, srv, "/transfers/"+testEnvelopeID+"/resolve", map[string]string{"rejected": "no"})
		require.Equal(t, http.StatusNotFound, rep.StatusCode)

		mockStore.AssertCalls(t, "PrepareTransaction", 1)
	})
}

// TestCallbackGuards covers the checks that stop a counterparty from moving a transfer
// it has no business moving, even when it holds a valid token.
func TestCallbackGuards(t *testing.T) {
	resolveToken := callback.Token(mustKey(t), testEnvelopeID, callback.PurposeResolve)
	confirmToken := callback.Token(mustKey(t), testEnvelopeID, callback.PurposeConfirm)

	counterpartyID := ulid.MakeSecure()

	testCases := []struct {
		name        string
		path        string
		body        any
		transaction *models.Transaction
		expected    int
	}{
		{
			name:        "resolve an inbound transfer",
			path:        "/transfers/" + testEnvelopeID + "/resolve/" + resolveToken,
			body:        map[string]string{"rejected": "no"},
			transaction: &models.Transaction{Source: enum.SourceRemote, Status: enum.StatusPending},
			expected:    http.StatusNotFound,
		},
		{
			name:        "resolve a transfer that is not pending",
			path:        "/transfers/" + testEnvelopeID + "/resolve/" + resolveToken,
			body:        map[string]string{"rejected": "no"},
			transaction: &models.Transaction{Source: enum.SourceLocal, Status: enum.StatusCompleted},
			expected:    http.StatusConflict,
		},
		{
			name:        "confirm an outbound transfer",
			path:        "/transfers/" + testEnvelopeID + "/confirm/" + confirmToken,
			body:        map[string]string{"txid": "0x1"},
			transaction: &models.Transaction{Source: enum.SourceLocal, Status: enum.StatusAccepted},
			expected:    http.StatusNotFound,
		},
		{
			name: "confirm a transfer that is not accepted",
			path: "/transfers/" + testEnvelopeID + "/confirm/" + confirmToken,
			body: map[string]string{"txid": "0x1"},
			transaction: &models.Transaction{
				Source:         enum.SourceRemote,
				Status:         enum.StatusReview,
				CounterpartyID: ulid.NullULID{Valid: true, ULID: counterpartyID},
			},
			expected: http.StatusConflict,
		},
		{
			name:        "confirm without a txid or cancellation",
			path:        "/transfers/" + testEnvelopeID + "/confirm/" + confirmToken,
			body:        map[string]string{},
			transaction: &models.Transaction{Source: enum.SourceRemote, Status: enum.StatusAccepted},
			expected:    http.StatusUnprocessableEntity,
		},
		{
			name:        "confirm with both a txid and a cancellation",
			path:        "/transfers/" + testEnvelopeID + "/confirm/" + confirmToken,
			body:        map[string]string{"txid": "0x1", "canceled": "nope"},
			transaction: &models.Transaction{Source: enum.SourceRemote, Status: enum.StatusAccepted},
			expected:    http.StatusUnprocessableEntity,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, mockStore := newTestServer(t, false)
			prepared(t, mockStore, tc.transaction)

			mockStore.OnRetrieveCounterparty = func(_ context.Context, id ulid.ULID) (*models.Counterparty, error) {
				return &models.Counterparty{
					Protocol:   enum.ProtocolTRP,
					CommonName: "counterparty.example.com",
					Endpoint:   "https://counterparty.example.com",
					Name:       "Counterparty VASP",
				}, nil
			}

			rep := post(t, srv, tc.path, tc.body)
			require.Equal(t, tc.expected, rep.StatusCode)
		})
	}
}
