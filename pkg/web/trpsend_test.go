package web

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// resolveCallbackPath has to leave the token bearing callbacks this node now hands out
// alone while still supporting the base callback convention of older Envoy nodes.
func TestResolveCallbackPath(t *testing.T) {
	const envelopeID = "5f1a6bcb-4e2b-4a9d-bc9f-3e9a2d3f5c11"

	testCases := []struct {
		name     string
		stored   string
		expected string
	}{
		{
			name:     "envoy base callback gets the resolve path appended",
			stored:   "https://originator.example.com/transfers/" + envelopeID,
			expected: "https://originator.example.com/transfers/" + envelopeID + "/resolve",
		},
		{
			name:     "trailing slash is tolerated",
			stored:   "https://originator.example.com/transfers/" + envelopeID + "/",
			expected: "https://originator.example.com/transfers/" + envelopeID + "/resolve",
		},
		{
			name:     "path prefixed base callback gets the resolve path appended",
			stored:   "https://gateway.example.com/tenant-a/transfers/" + envelopeID,
			expected: "https://gateway.example.com/tenant-a/transfers/" + envelopeID + "/resolve",
		},
		{
			name:     "token bearing callback is used verbatim",
			stored:   "https://originator.example.com/transfers/" + envelopeID + "/resolve/Zm9vYmFyYmF6",
			expected: "https://originator.example.com/transfers/" + envelopeID + "/resolve/Zm9vYmFyYmF6",
		},
		{
			name:     "tokenless resolve callback is used verbatim",
			stored:   "https://originator.example.com/transfers/" + envelopeID + "/resolve",
			expected: "https://originator.example.com/transfers/" + envelopeID + "/resolve",
		},
		{
			name:     "a callback with a different shape is used verbatim",
			stored:   "https://trpd.example.com/api/v3/callbacks/abc123",
			expected: "https://trpd.example.com/api/v3/callbacks/abc123",
		},
		{
			name:     "a callback for another transfer is not rewritten",
			stored:   "https://originator.example.com/transfers/9c4f0b2e-77a1-4c85-9f0e-0b1d2c3a4e5f",
			expected: "https://originator.example.com/transfers/9c4f0b2e-77a1-4c85-9f0e-0b1d2c3a4e5f",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			callback, err := resolveCallbackPath(tc.stored, envelopeID)
			require.NoError(t, err, "could not resolve the callback path")
			require.Equal(t, tc.expected, callback.String())
		})
	}

	t.Run("Invalid", func(t *testing.T) {
		_, err := resolveCallbackPath("://not a url", envelopeID)
		require.Error(t, err, "expected an unparseable callback to be an error")
	})
}
