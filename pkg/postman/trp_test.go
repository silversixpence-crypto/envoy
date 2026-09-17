package postman_test

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/trisacrypto/envoy/pkg/postman"
	"github.com/trisacrypto/trisa/pkg/ivms101"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
	api "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	generic "github.com/trisacrypto/trisa/pkg/trisa/data/generic/v1beta1"
)

const testEnvelopeID = "5f1a6bcb-4e2b-4a9d-bc9f-3e9a2d3f5c11"

// makeInquiry builds the inbound inquiry packet that Server.Inquiry resolves.
func makeInquiry(t *testing.T) *postman.TRPPacket {
	t.Helper()

	identity := &ivms101.IdentityPayload{
		Originator: &ivms101.Originator{
			AccountNumbers: []string{"mrfAEzGzK23kU23FxrToDRPmV1ReNfX43G"},
		},
		Beneficiary: &ivms101.Beneficiary{
			AccountNumbers: []string{"mtF8SmhQQcSJHLsZF9pSJQ5g4YeAJCuUqz"},
		},
	}

	inquiry := &trp.Inquiry{
		Info: &trp.Info{
			APIVersion:        "3.2.1",
			RequestIdentifier: testEnvelopeID,
		},
		Asset:    &trp.Asset{DTI: "4H95J0R2X"},
		Amount:   100000,
		Callback: "https://originator.example.com/transfers/" + testEnvelopeID + "/resolve/tok",
		IVMS101:  identity,
	}

	packet, err := postman.ReceiveTRPInquiry(inquiry, nil)
	require.NoError(t, err, "could not receive trp inquiry")

	packet.Log = zerolog.Nop()

	return packet
}

func TestTRPPacketResolve(t *testing.T) {
	t.Run("Pending", func(t *testing.T) {
		packet := makeInquiry(t)

		require.NoError(t, packet.Resolve(&trp.Resolution{Version: "3.2.1"}))
		require.False(t, packet.Out.Envelope.IsError(), "a pending resolution is not an error envelope")
		require.Equal(t, api.TransferPending, packet.Out.Envelope.TransferState())

		// The inquiry is echoed back unchanged while the transfer awaits review.
		payload, err := packet.Out.Envelope.Payload()
		require.NoError(t, err, "could not read the outgoing payload")

		msg := &generic.TRP{}
		require.NoError(t, payload.Transaction.UnmarshalTo(msg), "expected a trp message")
		require.NotNil(t, msg.GetInquiry(), "expected the inquiry to be echoed back")
	})

	t.Run("Approved", func(t *testing.T) {
		packet := makeInquiry(t)

		resolution := &trp.Resolution{
			Approved: &trp.Approval{
				Address:  "mtF8SmhQQcSJHLsZF9pSJQ5g4YeAJCuUqz",
				Callback: "https://beneficiary.example.com/transfers/" + testEnvelopeID + "/confirm/tok",
			},
		}

		require.NoError(t, packet.Resolve(resolution))
		require.False(t, packet.Out.Envelope.IsError())
		require.Equal(t, api.TransferAccepted, packet.Out.Envelope.TransferState())

		payload, err := packet.Out.Envelope.Payload()
		require.NoError(t, err, "could not read the outgoing payload")

		msg := &generic.TRP{}
		require.NoError(t, payload.Transaction.UnmarshalTo(msg), "expected a trp message")

		require.Equal(t, resolution.Approved.Address, msg.GetApproved().GetAddress())
		require.Equal(t, resolution.Approved.Callback, msg.GetApproved().GetCallback())

		// The headers and the reference transaction of the inquiry are carried forward.
		require.Equal(t, testEnvelopeID, msg.EnvelopeId)
		require.Equal(t, "3.2.1", msg.Headers.GetVersion())
		require.NotNil(t, msg.Transaction, "expected the reference transaction")
		require.Equal(t, "mtF8SmhQQcSJHLsZF9pSJQ5g4YeAJCuUqz", msg.Transaction.Beneficiary)
	})

	t.Run("Rejected", func(t *testing.T) {
		packet := makeInquiry(t)

		require.NoError(t, packet.Resolve(&trp.Resolution{Rejected: "sanctioned beneficiary"}))

		// A rejection is stored as an error envelope so the transaction reaches the
		// rejected status through the same path a TRISA rejection does.
		require.True(t, packet.Out.Envelope.IsError(), "expected an error envelope")
		require.Equal(t, api.TransferRejected, packet.Out.Envelope.TransferState())

		reject := packet.Out.Envelope.Error()
		require.Equal(t, api.Rejected, reject.Code)
		require.Equal(t, "sanctioned beneficiary", reject.Message)
		require.False(t, reject.Retry)
		require.Equal(t, testEnvelopeID, packet.Out.Envelope.ID())
	})
}

func TestPayloadFromResolution(t *testing.T) {
	identity, err := loadPayloadFixture("testdata/identity.pb.json", "testdata/transaction.pb.json")
	require.NoError(t, err, "could not load payload fixtures")

	t.Run("PreparedTransaction", func(t *testing.T) {
		// An outbound inquiry prepared by the web API carries a generic.Transaction
		// rather than a TRP message, and the resolution has to build on it too.
		payload, err := postman.PayloadFromResolution(identity, &trp.Resolution{
			Approved: &trp.Approval{Address: "n3Vgn8wF6ZkpKSe186NnytLPXdZ6j1JbHg", Callback: "https://example.com/confirm/tok"},
		})
		require.NoError(t, err, "could not build the resolution payload")

		msg := &generic.TRP{}
		require.NoError(t, payload.Transaction.UnmarshalTo(msg))
		require.Equal(t, "https://example.com/confirm/tok", msg.GetApproved().GetCallback())
		require.NotNil(t, msg.Transaction, "expected the prepared transaction to be carried over")
		require.Equal(t, identity.Identity, payload.Identity, "expected the identity to be carried over")
		require.NotEmpty(t, payload.ReceivedAt, "expected a received at timestamp on the reply")
	})

	t.Run("Rejected", func(t *testing.T) {
		payload, err := postman.PayloadFromResolution(identity, &trp.Resolution{Rejected: "no thanks"})
		require.NoError(t, err, "could not build the rejection payload")

		msg := &generic.TRP{}
		require.NoError(t, payload.Transaction.UnmarshalTo(msg))
		require.Equal(t, "no thanks", msg.GetRejected().GetRejected())
	})

	t.Run("Invalid", func(t *testing.T) {
		testCases := []struct {
			name string
			base *api.Payload
			res  *trp.Resolution
			err  error
		}{
			{"no payload", nil, &trp.Resolution{Rejected: "no"}, postman.ErrNoTRPPayload},
			{"no resolution", identity, nil, postman.ErrNoTRPResolution},
			{"version only", identity, &trp.Resolution{Version: "3.2.1"}, postman.ErrNoTRPResolution},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := postman.PayloadFromResolution(tc.base, tc.res)
				require.ErrorIs(t, err, tc.err)
			})
		}
	})
}

func TestPayloadFromConfirmation(t *testing.T) {
	base, err := loadPayloadFixture("testdata/identity.pb.json", "testdata/transaction.pb.json")
	require.NoError(t, err, "could not load payload fixtures")

	t.Run("Confirmed", func(t *testing.T) {
		payload, err := postman.PayloadFromConfirmation(base, &trp.Confirmation{TXID: "0xdeadbeef"})
		require.NoError(t, err, "could not build the confirmation payload")

		msg := &generic.TRP{}
		require.NoError(t, payload.Transaction.UnmarshalTo(msg))
		require.Equal(t, "0xdeadbeef", msg.GetConfirmed().GetTxid())
		require.Equal(t, "0xdeadbeef", msg.Transaction.GetTxid(), "expected the txid on the reference transaction")
	})

	t.Run("Canceled", func(t *testing.T) {
		payload, err := postman.PayloadFromConfirmation(base, &trp.Confirmation{Canceled: "sender changed their mind"})
		require.NoError(t, err, "could not build the cancellation payload")

		msg := &generic.TRP{}
		require.NoError(t, payload.Transaction.UnmarshalTo(msg))
		require.Equal(t, "sender changed their mind", msg.GetCanceled().GetCanceled())

		reference := &generic.Transaction{}
		require.NoError(t, base.Transaction.UnmarshalTo(reference))
		require.Equal(t, reference.Txid, msg.Transaction.GetTxid(), "a cancellation does not change the reference txid")
	})

	t.Run("Invalid", func(t *testing.T) {
		_, err := postman.PayloadFromConfirmation(base, &trp.Confirmation{})
		require.ErrorIs(t, err, postman.ErrNoTRPConfirmation)

		_, err = postman.PayloadFromConfirmation(nil, &trp.Confirmation{TXID: "0x1"})
		require.ErrorIs(t, err, postman.ErrNoTRPPayload)
	})
}

func TestReceiveResolution(t *testing.T) {
	// An approval received for an outbound inquiry has to keep the confirmation callback
	// the beneficiary named, because that URL is only ever sent once.
	t.Run("Approved", func(t *testing.T) {
		packet := makeInquiry(t)

		resolution := &trp.Resolution{
			Approved: &trp.Approval{
				Address:  "n3Vgn8wF6ZkpKSe186NnytLPXdZ6j1JbHg",
				Callback: "https://beneficiary.example.com/transfers/" + testEnvelopeID + "/confirm/tok",
			},
		}

		require.NoError(t, packet.ReceiveResolution(resolution))
		require.Equal(t, api.TransferAccepted, packet.In.Envelope.TransferState())

		payload, err := packet.In.Envelope.Payload()
		require.NoError(t, err, "could not read the incoming payload")

		msg := &generic.TRP{}
		require.NoError(t, payload.Transaction.UnmarshalTo(msg))
		require.Equal(t, resolution.Approved.Callback, msg.GetApproved().GetCallback())
	})

	t.Run("Pending", func(t *testing.T) {
		packet := makeInquiry(t)

		require.NoError(t, packet.ReceiveResolution(&trp.Resolution{Version: "3.2.1"}))
		require.Equal(t, api.TransferPending, packet.In.Envelope.TransferState())

		payload, err := packet.In.Envelope.Payload()
		require.NoError(t, err, "could not read the incoming payload")

		msg := &generic.TRP{}
		require.NoError(t, payload.Transaction.UnmarshalTo(msg))
		require.NotNil(t, msg.GetInquiry(), "a pending resolution keeps the inquiry")
	})
}
