package client_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	trpclient "github.com/trisacrypto/envoy/pkg/trp/client"
	"github.com/trisacrypto/trisa/pkg/openvasp"
	"github.com/trisacrypto/trisa/pkg/openvasp/client"
	"github.com/trisacrypto/trisa/pkg/openvasp/extensions/discoverability"
)

// The behaviour under test is the whole point of this package, so the test documents
// both halves of it: the trisa library's own client cannot read a compressed reply, and
// the client this package builds can.
func TestStripAcceptEncoding(t *testing.T) {
	ctx := context.Background()
	fixture := &discoverability.Version{Version: "3.2.1", Vendor: "TRISA Envoy"}

	t.Run("LibraryClientFails", func(t *testing.T) {
		srv, seen := cdn(t, fixture)

		trpc, err := client.New()
		require.NoError(t, err, "could not create the library trp client")

		_, err = trpc.Version(ctx, srv.URL)
		require.Error(t, err, "the library client is expected to choke on a gzipped reply")
		require.Contains(t, err.Error(), "invalid character '\\x1f'", "expected a json decode error on the gzip magic bytes")

		// The library asks for encodings it cannot read, which is what makes the CDN
		// compress in the first place.
		require.Equal(t, "gzip, deflate, br", seen(), "expected the library's hardcoded accept-encoding")
	})

	t.Run("HelperDecodes", func(t *testing.T) {
		srv, seen := cdn(t, fixture)

		trpc, err := trpclient.New()
		require.NoError(t, err, "could not create the envoy trp client")

		version, err := trpc.Version(ctx, srv.URL)
		require.NoError(t, err, "could not read the gzipped version reply")
		require.Equal(t, fixture, version, "the reply was not decoded")

		// net/http only decompresses transparently for a request it added the header to
		// itself, so seeing exactly "gzip" is the evidence that the wrapper removed the
		// library's header before the transport looked at it.
		require.Equal(t, "gzip", seen(), "expected the transport's own accept-encoding")
	})

	t.Run("UncompressedReply", func(t *testing.T) {
		srv, _ := cdn(t, fixture)

		trpc, err := trpclient.New()
		require.NoError(t, err, "could not create the envoy trp client")

		// A node with no CDN in front of it answers in plaintext; the transport passes
		// that through untouched.
		srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(openvasp.ContentTypeHeader, openvasp.ContentTypeValue)
			json.NewEncoder(w).Encode(fixture)
		})

		version, err := trpc.Version(ctx, srv.URL)
		require.NoError(t, err, "could not read the plaintext version reply")
		require.Equal(t, fixture, version, "the reply was not decoded")
	})
}

// A client without a cookie jar drops CSRF cookies, and WithClient replaces the jar the
// library would have created, so check the helper put one back.
func TestCookieJarPreserved(t *testing.T) {
	var seen string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-CSRF-TOKEN")

		http.SetCookie(w, &http.Cookie{Name: "csrf_token", Value: "the-csrf-token", Path: "/"})
		w.Header().Set(openvasp.ContentTypeHeader, openvasp.ContentTypeValue)
		json.NewEncoder(w).Encode(&discoverability.Version{Version: "3.2.1"})
	}))

	t.Cleanup(srv.Close)

	trpc, err := trpclient.New()
	require.NoError(t, err, "could not create the envoy trp client")

	_, err = trpc.Version(context.Background(), srv.URL)
	require.NoError(t, err, "could not execute the first request")
	require.Empty(t, seen, "there was no cookie to send on the first request")

	_, err = trpc.Version(context.Background(), srv.URL)
	require.NoError(t, err, "could not execute the second request")
	require.Equal(t, "the-csrf-token", seen, "the cookie jar did not carry the csrf cookie over")
}

// cdn stands in for Cloudflare in front of a TRP node: it serves the discoverability
// version endpoint and gzips the reply whenever the request says it accepts gzip.
// It also reports the Accept-Encoding header of the last request it handled.
func cdn(t *testing.T, version *discoverability.Version) (srv *httptest.Server, seen func() string) {
	t.Helper()

	var accept string

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept-Encoding")

		if r.URL.Path != discoverability.VersionEndpoint {
			http.Error(w, "expected the version endpoint, got "+r.URL.Path, http.StatusNotFound)
			return
		}

		w.Header().Set(openvasp.ContentTypeHeader, openvasp.ContentTypeValue)

		if !strings.Contains(accept, "gzip") {
			json.NewEncoder(w).Encode(version)
			return
		}

		w.Header().Set("Content-Encoding", "gzip")

		gz := gzip.NewWriter(w)
		defer gz.Close()

		json.NewEncoder(gz).Encode(version)
	}))

	t.Cleanup(srv.Close)

	return srv, func() string { return accept }
}
