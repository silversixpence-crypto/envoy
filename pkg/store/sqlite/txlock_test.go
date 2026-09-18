package sqlite

import (
	"net/url"
	"testing"

	"github.com/trisacrypto/envoy/pkg/store/dsn"

	"github.com/stretchr/testify/require"
)

// The write pool is the only thing that separates a write transaction's BEGIN IMMEDIATE
// from a read transaction's plain BEGIN, and the only place that distinction can be made
// is the connection string, so the rendered parameters are worth asserting directly.
func TestTxLockParams(t *testing.T) {
	tests := []struct {
		name   string
		uri    string
		write  bool
		txlock string
	}{
		{
			name:   "ReadPoolStaysDeferred",
			uri:    "sqlite3:///test.db",
			write:  false,
			txlock: "",
		},
		{
			name:   "WritePoolIsImmediate",
			uri:    "sqlite3:///test.db",
			write:  true,
			txlock: "immediate",
		},
		{
			name:   "ReadOnlyHasNoTxLock",
			uri:    "sqlite3:///test.db?readonly=true",
			write:  false,
			txlock: "",
		},
		{
			name:   "DsnOverridesWritePool",
			uri:    "sqlite3:///test.db?_txlock=deferred",
			write:  true,
			txlock: "deferred",
		},
		{
			name:   "DsnReachesReadPool",
			uri:    "sqlite3:///test.db?_txlock=exclusive",
			write:  false,
			txlock: "exclusive",
		},
		{
			name:   "DsnIsCanonicalized",
			uri:    "sqlite3:///test.db?_TXLOCK=deferred",
			write:  true,
			txlock: "deferred",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			uri, err := dsn.Parse(tc.uri)
			require.NoError(t, err, "could not parse dsn")

			params, err := url.ParseQuery(connectionParams(uri, tc.write))
			require.NoError(t, err, "could not parse the rendered connection parameters")

			// The driver reads the parameter case-sensitively under this exact name, so
			// an override rendered under any other spelling would silently do nothing.
			require.Equal(t, tc.txlock, params.Get(txlockParam), "unexpected %s parameter", txlockParam)

			if tc.txlock == "" {
				require.NotContains(t, params, txlockParam, "expected no %s parameter at all", txlockParam)
			} else {
				require.Len(t, params[txlockParam], 1, "expected exactly one %s parameter", txlockParam)
			}

			// The split must not disturb the replication defaults on either pool.
			require.Equal(t, "5000", params.Get("_busy_timeout"), "expected the busy timeout on both pools")
			require.Equal(t, "on", params.Get("_foreign_keys"), "expected foreign keys on both pools")
		})
	}
}
