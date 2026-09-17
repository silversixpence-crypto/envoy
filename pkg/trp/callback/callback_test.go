package callback_test

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/trisacrypto/envoy/pkg/trp/callback"
)

const (
	envelopeID = "5f1a6bcb-4e2b-4a9d-bc9f-3e9a2d3f5c11"
	otherID    = "9c4f0b2e-77a1-4c85-9f0e-0b1d2c3a4e5f"
)

func key(t *testing.T) []byte {
	t.Helper()

	k, err := hex.DecodeString("b1f8ad0a9a1c32ec19a24b0cbc1e5b0fdc5dd3ad4ee62b9b09b8b3b8fdd4a9c7")
	require.NoError(t, err, "could not decode the test callback key")

	return k
}

func TestToken(t *testing.T) {
	k := key(t)

	t.Run("Deterministic", func(t *testing.T) {
		require.Equal(t, callback.Token(k, envelopeID, callback.PurposeResolve), callback.Token(k, envelopeID, callback.PurposeResolve))
	})

	t.Run("URLSafe", func(t *testing.T) {
		token := callback.Token(k, envelopeID, callback.PurposeResolve)

		raw, err := base64.RawURLEncoding.DecodeString(token)
		require.NoError(t, err, "expected an unpadded base64url token")
		require.Len(t, raw, 32, "expected a sha256 sized token")
		require.NotContains(t, token, "=", "expected no padding in the token")
	})

	t.Run("Distinct", func(t *testing.T) {
		other, _ := hex.DecodeString("00f8ad0a9a1c32ec19a24b0cbc1e5b0fdc5dd3ad4ee62b9b09b8b3b8fdd4a9c7")

		testCases := []struct {
			name    string
			key     []byte
			id      string
			purpose string
		}{
			{"different transfer", k, otherID, callback.PurposeResolve},
			{"different purpose", k, envelopeID, callback.PurposeConfirm},
			{"different key", other, envelopeID, callback.PurposeResolve},
		}

		token := callback.Token(k, envelopeID, callback.PurposeResolve)

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				require.NotEqual(t, token, callback.Token(tc.key, tc.id, tc.purpose))
			})
		}
	})
}

func TestVerify(t *testing.T) {
	k := key(t)
	resolve := callback.Token(k, envelopeID, callback.PurposeResolve)
	confirm := callback.Token(k, envelopeID, callback.PurposeConfirm)

	testCases := []struct {
		name     string
		key      []byte
		id       string
		purpose  string
		token    string
		expected bool
	}{
		{"resolve token", k, envelopeID, callback.PurposeResolve, resolve, true},
		{"confirm token", k, envelopeID, callback.PurposeConfirm, confirm, true},
		{"wrong purpose", k, envelopeID, callback.PurposeConfirm, resolve, false},
		{"wrong transfer", k, otherID, callback.PurposeResolve, resolve, false},
		{"empty token", k, envelopeID, callback.PurposeResolve, "", false},
		{"garbage token", k, envelopeID, callback.PurposeResolve, "not-a-token", false},
		{"no key", nil, envelopeID, callback.PurposeResolve, resolve, false},
		{"no key and no token", nil, envelopeID, callback.PurposeResolve, "", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, callback.Verify(tc.key, tc.id, tc.purpose, tc.token))
		})
	}
}

func TestCallbackURL(t *testing.T) {
	k := key(t)
	resolve := callback.Token(k, envelopeID, callback.PurposeResolve)

	testCases := []struct {
		name     string
		endpoint string
		scheme   string
		purpose  string
		key      []byte
		expected string
	}{
		{
			name:     "host only",
			endpoint: "trp.example.com",
			scheme:   "https",
			purpose:  callback.PurposeResolve,
			key:      k,
			expected: "https://trp.example.com/transfers/" + envelopeID + "/resolve/" + resolve,
		},
		{
			name:     "scheme stripped from the endpoint",
			endpoint: "https://trp.example.com:8200",
			scheme:   "http",
			purpose:  callback.PurposeResolve,
			key:      k,
			expected: "http://trp.example.com:8200/transfers/" + envelopeID + "/resolve/" + resolve,
		},
		{
			name:     "path prefix preserved",
			endpoint: "trp.example.com/tenant-a/",
			scheme:   "https",
			purpose:  callback.PurposeConfirm,
			key:      k,
			expected: "https://trp.example.com/tenant-a/transfers/" + envelopeID + "/confirm/" + callback.Token(k, envelopeID, callback.PurposeConfirm),
		},
		{
			name:     "no key yields the tokenless url",
			endpoint: "http://trp.example.com",
			scheme:   "http",
			purpose:  callback.PurposeResolve,
			key:      nil,
			expected: "http://trp.example.com/transfers/" + envelopeID + "/resolve",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, callback.CallbackURL(tc.endpoint, tc.scheme, envelopeID, tc.purpose, tc.key))
		})
	}
}
