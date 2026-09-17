package webhook_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/trisacrypto/envoy/pkg/webhook"
	"github.com/trisacrypto/trisa/pkg/ivms101"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	generic "github.com/trisacrypto/trisa/pkg/trisa/data/generic/v1beta1"
	"google.golang.org/protobuf/types/known/anypb"
)

// AddPayload has to accept the generic.TRP messages that inbound TRP inquiries produce,
// exposing the TRP message itself while still filling in the reference transaction that
// callbacks written for TRISA transfers read.
func TestAddPayloadTRP(t *testing.T) {
	identity, err := anypb.New(&ivms101.IdentityPayload{})
	require.NoError(t, err, "could not marshal identity payload")

	makePayload := func(t *testing.T, transaction any) *trisa.Payload {
		t.Helper()

		var (
			anyval *anypb.Any
			err    error
		)

		switch msg := transaction.(type) {
		case *generic.TRP:
			anyval, err = anypb.New(msg)
		case *generic.Transaction:
			anyval, err = anypb.New(msg)
		case *generic.Pending:
			anyval, err = anypb.New(msg)
		}

		require.NoError(t, err, "could not marshal transaction payload")

		return &trisa.Payload{
			Identity:    identity,
			Transaction: anyval,
			SentAt:      "2026-09-17T08:00:00Z",
		}
	}

	t.Run("Inquiry", func(t *testing.T) {
		payload := makePayload(t, &generic.TRP{
			EnvelopeId: "5f1a6bcb-4e2b-4a9d-bc9f-3e9a2d3f5c11",
			Headers:    &generic.TRPInfo{Version: "3.2.1"},
			Message: &generic.TRP_Inquiry{
				Inquiry: &generic.TRPInquiry{
					Amount:   100000,
					Callback: "https://originator.example.com/transfers/5f1a6bcb-4e2b-4a9d-bc9f-3e9a2d3f5c11/resolve/abc",
				},
			},
			Transaction: &generic.Transaction{
				Originator:  "mkX5x4vmLrB5GmFBoqDGHPnYchWhfoSuJP",
				Beneficiary: "mtF8SmhQQcSJHLsZF9pSJQ5g4YeAJCuUqz",
				Amount:      100000,
				Network:     "BTC",
			},
		})

		req := &webhook.Request{}
		require.NoError(t, req.AddPayload(payload), "could not add a trp payload to the request")

		require.NotNil(t, req.Payload.TRP, "expected the trp message on the payload")
		require.Equal(t, "https://originator.example.com/transfers/5f1a6bcb-4e2b-4a9d-bc9f-3e9a2d3f5c11/resolve/abc", req.Payload.TRP.GetInquiry().GetCallback())

		require.NotNil(t, req.Payload.Transaction, "expected the reference transaction on the payload")
		require.Equal(t, "mtF8SmhQQcSJHLsZF9pSJQ5g4YeAJCuUqz", req.Payload.Transaction.Beneficiary)

		require.Nil(t, req.Payload.Pending)
		require.Nil(t, req.Payload.Sunrise)
		require.False(t, req.Payload.IsZero())
	})

	t.Run("Approved", func(t *testing.T) {
		payload := makePayload(t, &generic.TRP{
			Message: &generic.TRP_Approved{
				Approved: &generic.TRPApproved{
					Address:  "mtF8SmhQQcSJHLsZF9pSJQ5g4YeAJCuUqz",
					Callback: "https://beneficiary.example.com/transfers/id/confirm/tok",
				},
			},
		})

		req := &webhook.Request{}
		require.NoError(t, req.AddPayload(payload), "could not add a trp approval to the request")

		require.Equal(t, "https://beneficiary.example.com/transfers/id/confirm/tok", req.Payload.TRP.GetApproved().GetCallback())

		// A TRP message without a reference transaction leaves Transaction unset rather
		// than filling it with an empty message.
		require.Nil(t, req.Payload.Transaction, "did not expect a reference transaction")
	})

	t.Run("Transaction", func(t *testing.T) {
		payload := makePayload(t, &generic.Transaction{Network: "BTC", Amount: 0.001})

		req := &webhook.Request{}
		require.NoError(t, req.AddPayload(payload), "could not add a transaction payload to the request")

		require.Nil(t, req.Payload.TRP, "did not expect a trp message on a transaction payload")
		require.Equal(t, "BTC", req.Payload.Transaction.Network)
	})

	t.Run("Unknown", func(t *testing.T) {
		payload := &trisa.Payload{Identity: identity, Transaction: &anypb.Any{TypeUrl: "type.googleapis.com/not.a.Type"}}

		req := &webhook.Request{}
		require.EqualError(t, req.AddPayload(payload), `unknown transaction type "type.googleapis.com/not.a.Type"`)
	})
}
