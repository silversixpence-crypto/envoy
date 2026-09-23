package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/trisacrypto/envoy/pkg/bufconn"
	"github.com/trisacrypto/envoy/pkg/config"
	"github.com/trisacrypto/envoy/pkg/enum"
	"github.com/trisacrypto/envoy/pkg/store"
	storemock "github.com/trisacrypto/envoy/pkg/store/mock"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/envoy/pkg/trisa/network"
	api "github.com/trisacrypto/envoy/pkg/web/api/v1"
	"github.com/trisacrypto/trisa/pkg/ivms101"
	"github.com/trisacrypto/trisa/pkg/openvasp"
	generic "github.com/trisacrypto/trisa/pkg/trisa/data/generic/v1beta1"
	"go.rtnl.ai/ulid"
)

const (
	sendTestEnvelopeID  = "5f1a6bcb-4e2b-4a9d-bc9f-3e9a2d3f5c11"
	sendTestCallbackKey = "b1f8ad0a9a1c32ec19a24b0cbc1e5b0fdc5dd3ad4ee62b9b09b8b3b8fdd4a9c7"
)

// TestSendPreparedEnvelopeID covers the caller supplied envelope id on send-prepared:
// the default path is unchanged, a supplied id is used everywhere the transfer is
// identified, a repeat of the request returns the first transaction without a second
// TRP inquiry, and an id that belongs to some other transaction is a conflict.
func TestSendPreparedEnvelopeID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("Default", func(t *testing.T) {
		f := newSendFixture(t)

		rep := f.send(t, f.prepared(""))
		require.Equal(t, http.StatusCreated, rep.Code, rep.Body.String())

		out := decodeTransaction(t, rep)
		require.Equal(t, uuid.Version(4), out.ID.Version(), "expected the node to generate a v4 uuid")
		require.Equal(t, 1, f.cp.count(), "expected exactly one trp inquiry")
		require.Equal(t, out.ID.String(), f.cp.last().requestID)

		f.assertStored(t, out.ID)
	})

	t.Run("CallerID", func(t *testing.T) {
		f := newSendFixture(t)

		rep := f.send(t, f.prepared(sendTestEnvelopeID))
		require.Equal(t, http.StatusCreated, rep.Code, rep.Body.String())

		out := decodeTransaction(t, rep)
		require.Equal(t, sendTestEnvelopeID, out.ID.String())
		require.Equal(t, "local", out.Source)
		require.Equal(t, "pending", out.Status)

		// The counterparty sees the caller's id as the request identifier and in the
		// callback it posts its resolution to, which is what the resolution correlates on.
		require.Equal(t, 1, f.cp.count(), "expected exactly one trp inquiry")
		inquiry := f.cp.last()
		require.Equal(t, sendTestEnvelopeID, inquiry.requestID)
		require.Contains(t, inquiry.callback, "/transfers/"+sendTestEnvelopeID+"/resolve/")

		f.assertStored(t, out.ID)
	})

	t.Run("Repeat", func(t *testing.T) {
		f := newSendFixture(t)

		first := f.send(t, f.prepared(sendTestEnvelopeID))
		require.Equal(t, http.StatusCreated, first.Code, first.Body.String())

		// The counterparty's decision may land between the attempts; the retry reports
		// the transaction as it is now.
		f.db.transactions[uuid.MustParse(sendTestEnvelopeID)].Status = enum.StatusAccepted

		second := f.send(t, f.prepared(sendTestEnvelopeID))
		require.Equal(t, http.StatusOK, second.Code, second.Body.String())

		original := decodeTransaction(t, first)
		repeated := decodeTransaction(t, second)
		require.Equal(t, original.ID, repeated.ID)
		require.Equal(t, original.Counterparty, repeated.Counterparty)
		require.Equal(t, original.CounterpartyID, repeated.CounterpartyID)
		require.Equal(t, original.Amount, repeated.Amount)
		require.Equal(t, "accepted", repeated.Status)

		require.Equal(t, 1, f.cp.count(), "a repeated request must not send a second trp inquiry")
		require.Len(t, f.db.transactions, 1, "a repeated request must not create a second transaction")
		require.Len(t, f.db.envelopes[original.ID], 2, "a repeated request must not store more envelopes")
	})

	t.Run("ConflictInbound", func(t *testing.T) {
		f := newSendFixture(t)

		// An inbound transfer that happens to have the same id.
		id := uuid.MustParse(sendTestEnvelopeID)
		f.db.transactions[id] = &models.Transaction{
			ID:             id,
			Source:         enum.SourceRemote,
			Status:         enum.StatusReview,
			Counterparty:   f.counterparty.Name,
			CounterpartyID: ulid.NullULID{Valid: true, ULID: f.counterparty.ID},
		}

		rep := f.send(t, f.prepared(sendTestEnvelopeID))
		require.Equal(t, http.StatusConflict, rep.Code, rep.Body.String())
		require.Contains(t, rep.Body.String(), "envelope_id is already in use")
		require.Equal(t, 0, f.cp.count(), "a conflict must not send a trp inquiry")
		require.Equal(t, enum.SourceRemote, f.db.transactions[id].Source, "the existing transaction must not change")
	})

	t.Run("ConflictDifferentTransfer", func(t *testing.T) {
		f := newSendFixture(t)

		rep := f.send(t, f.prepared(sendTestEnvelopeID))
		require.Equal(t, http.StatusCreated, rep.Code, rep.Body.String())

		// Reusing the id for a different transfer must not return the first one.
		other := f.prepared(sendTestEnvelopeID)
		other.Transaction.Amount = 1.5

		rep = f.send(t, other)
		require.Equal(t, http.StatusConflict, rep.Code, rep.Body.String())
		require.Equal(t, 1, f.cp.count(), "a conflict must not send a trp inquiry")
	})

	t.Run("ConflictDifferentAsset", func(t *testing.T) {
		f := newSendFixture(t)

		rep := f.send(t, f.prepared(sendTestEnvelopeID))
		require.Equal(t, http.StatusCreated, rep.Code, rep.Body.String())

		// Same id, addresses and amount, but a different asset is a different transfer.
		other := f.prepared(sendTestEnvelopeID)
		other.Transaction.Network = "ETH"

		rep = f.send(t, other)
		require.Equal(t, http.StatusConflict, rep.Code, rep.Body.String())
		require.Equal(t, 1, f.cp.count(), "a conflict must not send a trp inquiry")
	})

	t.Run("CallerDisconnects", func(t *testing.T) {
		f := newSendFixture(t)

		// The caller gives up and disconnects while the counterparty is still answering,
		// which cancels the request context.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.cp.onInquiry = cancel

		f.sendWithContext(t, ctx, f.prepared(sendTestEnvelopeID))
		require.Error(t, ctx.Err(), "expected the request context to be cancelled during the send")

		// The send still completed and committed under the caller's id.
		require.Equal(t, 1, f.cp.count())
		f.assertStored(t, uuid.MustParse(sendTestEnvelopeID))

		// The retry learns the outcome instead of sending a second inquiry.
		f.cp.onInquiry = nil
		rep := f.send(t, f.prepared(sendTestEnvelopeID))
		require.Equal(t, http.StatusOK, rep.Code, rep.Body.String())
		require.Equal(t, sendTestEnvelopeID, decodeTransaction(t, rep).ID.String())
		require.Equal(t, 1, f.cp.count(), "the retry must not send a second trp inquiry")
		require.Len(t, f.db.transactions, 1)
	})

	t.Run("InvalidID", func(t *testing.T) {
		for _, id := range []string{"travel-rule:123:dispatch", "6ba7b810-9dad-11d1-80b4-00c04fd430c8"} {
			f := newSendFixture(t)

			rep := f.send(t, f.prepared(id))
			require.Equal(t, http.StatusBadRequest, rep.Code, rep.Body.String())
			require.Contains(t, rep.Body.String(), "envelope_id")
			require.Equal(t, 0, f.cp.count())
			f.store.AssertCalls(t, "PrepareTransaction", 0)
		}
	})
}

//===========================================================================
// Fixture
//===========================================================================

type sendFixture struct {
	srv          *Server
	store        *storemock.Store
	db           *sendDB
	cp           *trpCounterparty
	counterparty *models.Counterparty
}

func newSendFixture(t *testing.T) *sendFixture {
	t.Helper()

	f := &sendFixture{
		db: &sendDB{
			transactions: make(map[uuid.UUID]*models.Transaction),
			envelopes:    make(map[uuid.UUID][]*models.SecureEnvelope),
		},
		cp: &trpCounterparty{},
	}

	cpsrv := httptest.NewServer(f.cp)
	t.Cleanup(cpsrv.Close)

	f.counterparty = &models.Counterparty{
		Model:      models.Model{ID: ulid.MakeSecure()},
		Protocol:   enum.ProtocolTRP,
		CommonName: "beneficiary.example.com",
		Endpoint:   cpsrv.URL + "/transfers",
		Name:       "Beneficiary VASP",
	}

	db, err := store.Open("mock:///")
	require.NoError(t, err, "could not open the mock store")
	f.store = db.(*storemock.Store)

	f.store.OnRetrieveCounterparty = func(_ context.Context, id ulid.ULID) (*models.Counterparty, error) {
		if id != f.counterparty.ID {
			return nil, errors.New("unknown counterparty")
		}

		return f.counterparty, nil
	}

	f.store.OnPrepareTransaction = f.db.prepare

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

	f.srv = &Server{
		conf: config.Config{
			TRP: config.TRPConfig{
				Enabled:     true,
				Endpoint:    "originator.example.com",
				CallbackKey: sendTestCallbackKey,
			},
		},
		store: f.store,
		trisa: net,
	}

	return f
}

// prepared returns a send-prepared request body for a TRP transfer to the counterparty.
func (f *sendFixture) prepared(envelopeID string) *api.Prepared {
	originator := &api.Person{Forename: "Alice", Surname: "Originator", CryptoAddress: "mjJyX9R1nXw5ogdwSUc2F8WL4xYBn3nJVE", Identification: &api.Identification{}}
	beneficiary := &api.Person{Forename: "Bob", Surname: "Beneficiary", CryptoAddress: "mvE3W7Z3V5wRCXFcJgTa6BQhcEjJ5sUJPP", Identification: &api.Identification{}}

	return &api.Prepared{
		EnvelopeID: envelopeID,
		Routing:    &api.Routing{Protocol: "trp", CounterpartyID: f.counterparty.ID},
		Identity: &ivms101.IdentityPayload{
			Originator: &ivms101.Originator{
				OriginatorPersons: []*ivms101.Person{originator.NaturalPerson()},
				AccountNumbers:    []string{originator.CryptoAddress},
			},
			Beneficiary: &ivms101.Beneficiary{
				BeneficiaryPersons: []*ivms101.Person{beneficiary.NaturalPerson()},
				AccountNumbers:     []string{beneficiary.CryptoAddress},
			},
		},
		Transaction: &generic.Transaction{
			Originator:  originator.CryptoAddress,
			Beneficiary: beneficiary.CryptoAddress,
			Amount:      0.25,
			Network:     "BTC",
		},
	}
}

// send calls the send-prepared handler directly, bypassing authentication.
func (f *sendFixture) send(t *testing.T, in *api.Prepared) *httptest.ResponseRecorder {
	t.Helper()

	return f.sendWithContext(t, context.Background(), in)
}

// sendWithContext is send with the given request context, standing in for the context
// the http server cancels when the caller disconnects.
func (f *sendFixture) sendWithContext(t *testing.T, ctx context.Context, in *api.Prepared) *httptest.ResponseRecorder {
	t.Helper()

	data, err := json.Marshal(in)
	require.NoError(t, err, "could not marshal the prepared transaction")

	rep := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rep)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/transactions/send-prepared", bytes.NewReader(data))
	c.Request.Header.Set("Content-Type", "application/json")

	f.srv.SendPreparedTransaction(c)

	return rep
}

// assertStored checks that the committed transaction and every envelope stored with it
// carry the id the caller was given.
func (f *sendFixture) assertStored(t *testing.T, id uuid.UUID) {
	t.Helper()

	require.Len(t, f.db.transactions, 1)
	require.Contains(t, f.db.transactions, id)
	require.Equal(t, enum.SourceLocal, f.db.transactions[id].Source)

	envelopes := f.db.envelopes[id]
	require.Len(t, envelopes, 2, "expected the outgoing inquiry and the incoming resolution")

	for _, env := range envelopes {
		require.Equal(t, id, env.EnvelopeID)
		require.Equal(t, id.String(), env.Envelope.Id)
	}
}

func decodeTransaction(t *testing.T, rep *httptest.ResponseRecorder) *api.Transaction {
	t.Helper()

	out := &api.Transaction{}
	require.NoError(t, json.Unmarshal(rep.Body.Bytes(), out), "could not decode the transaction")

	return out
}

//===========================================================================
// TRP counterparty
//===========================================================================

type receivedInquiry struct {
	requestID string
	callback  string
}

// trpCounterparty records the inquiries it receives and answers each with a bare
// acknowledgement, which leaves the transfer pending as an asynchronous peer would.
// If set, onInquiry runs after an inquiry is recorded and before it is answered.
type trpCounterparty struct {
	sync.Mutex
	inquiries []receivedInquiry
	onInquiry func()
}

func (p *trpCounterparty) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	msg := struct {
		Callback string `json:"callback"`
	}{}
	_ = json.Unmarshal(body, &msg)

	p.Lock()
	p.inquiries = append(p.inquiries, receivedInquiry{
		requestID: r.Header.Get(openvasp.RequestIdentifierHeader),
		callback:  msg.Callback,
	})
	hook := p.onInquiry
	p.Unlock()

	if hook != nil {
		hook()
	}

	w.Header().Set(openvasp.ContentTypeHeader, openvasp.MIMEJSON)
	w.Header().Set(openvasp.APIVersionHeader, "3.2.1")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func (p *trpCounterparty) count() int {
	p.Lock()
	defer p.Unlock()

	return len(p.inquiries)
}

func (p *trpCounterparty) last() receivedInquiry {
	p.Lock()
	defer p.Unlock()

	return p.inquiries[len(p.inquiries)-1]
}

//===========================================================================
// In memory transactions table
//===========================================================================

// sendDB stands in for the transactions table: like a real database transaction, a
// prepared transaction is only visible to later requests once it is committed, and it
// is rolled back if the context it was begun with is done before it commits.
type sendDB struct {
	transactions map[uuid.UUID]*models.Transaction
	envelopes    map[uuid.UUID][]*models.SecureEnvelope
}

func (d *sendDB) prepare(ctx context.Context, id uuid.UUID, _ *models.ComplianceAuditLog) (models.PreparedTransaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if existing, ok := d.transactions[id]; ok {
		txn := *existing
		return &sendPrepared{ctx: ctx, db: d, txn: &txn}, nil
	}

	now := time.Now()

	return &sendPrepared{
		ctx:     ctx,
		db:      d,
		created: true,
		txn: &models.Transaction{
			ID:           id,
			Source:       enum.SourceUnknown,
			Status:       enum.StatusDraft,
			Counterparty: models.CounterpartyUnknown,
			VirtualAsset: models.VirtualAssetUnknown,
			Created:      now,
			Modified:     now,
		},
	}, nil
}

type sendPrepared struct {
	ctx       context.Context
	db        *sendDB
	txn       *models.Transaction
	envelopes []*models.SecureEnvelope
	created   bool
	done      bool
}

var errNotImplemented = errors.New("not implemented by the send test store")

func (p *sendPrepared) Created() bool { return p.created }

// finished reports whether the transaction is over, rolling it back if its context is
// done, which is what database/sql does to a transaction begun with that context.
func (p *sendPrepared) finished() bool {
	if !p.done && p.ctx.Err() != nil {
		p.done = true
	}

	return p.done
}

func (p *sendPrepared) Fetch() (*models.Transaction, error) {
	if p.finished() {
		return nil, sql.ErrTxDone
	}

	txn := *p.txn
	return &txn, nil
}

func (p *sendPrepared) Update(in *models.Transaction, _ *models.ComplianceAuditLog) error {
	if p.finished() {
		return sql.ErrTxDone
	}

	if in.ID != uuid.Nil && in.ID != p.txn.ID {
		return errors.New("transaction id mismatch")
	}

	p.txn.Update(in)
	p.txn.Modified = time.Now()

	return nil
}

func (p *sendPrepared) AddCounterparty(in *models.Counterparty, _ *models.ComplianceAuditLog) error {
	if p.finished() {
		return sql.ErrTxDone
	}

	p.txn.Counterparty = in.Name
	p.txn.CounterpartyID = ulid.NullULID{Valid: true, ULID: in.ID}

	return nil
}

func (p *sendPrepared) AddEnvelope(in *models.SecureEnvelope, _ *models.ComplianceAuditLog) error {
	if p.finished() {
		return sql.ErrTxDone
	}

	if in.EnvelopeID != p.txn.ID {
		return errors.New("envelope does not belong to the prepared transaction")
	}

	p.envelopes = append(p.envelopes, in)

	return nil
}

func (p *sendPrepared) UpdateCounterparty(*models.Counterparty, *models.ComplianceAuditLog) error {
	return errNotImplemented
}

func (p *sendPrepared) LookupCounterparty(string, string) (*models.Counterparty, error) {
	return nil, errNotImplemented
}

func (p *sendPrepared) CreateSunrise(*models.Sunrise, *models.ComplianceAuditLog) error {
	return errNotImplemented
}

func (p *sendPrepared) UpdateSunrise(*models.Sunrise, *models.ComplianceAuditLog) error {
	return errNotImplemented
}

func (p *sendPrepared) UpdateSunriseStatus(uuid.UUID, enum.Status, *models.ComplianceAuditLog) error {
	return errNotImplemented
}

func (p *sendPrepared) Rollback() error {
	if p.finished() {
		return sql.ErrTxDone
	}

	p.done = true

	return nil
}

func (p *sendPrepared) Commit() error {
	if p.finished() {
		return sql.ErrTxDone
	}

	p.done = true
	p.db.transactions[p.txn.ID] = p.txn
	p.db.envelopes[p.txn.ID] = append(p.db.envelopes[p.txn.ID], p.envelopes...)

	return nil
}

// Guard against the fake drifting from the interface it stands in for.
var _ models.PreparedTransaction = (*sendPrepared)(nil)
