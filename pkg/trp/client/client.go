/*
Package client constructs the outbound TRP client used everywhere this node talks to a
counterparty, wrapping github.com/trisacrypto/trisa/pkg/openvasp/client so that a
compressed response cannot break it.

The trisa client sets "Accept-Encoding: gzip, deflate, br" on every request and then
hands the response body straight to a json.Decoder, so it asks for compression it cannot
read. Against a bare TRP node nothing happens, because nothing compresses. Against a node
behind a CDN it breaks: Cloudflare, which fronts the hosted deployment, takes the header
at its word and gzips the JSON, and the decode fails on "invalid character '\x1f'".

net/http already handles this, but only for callers that leave Accept-Encoding alone: in
that case the transport adds its own "Accept-Encoding: gzip", decompresses the reply and
reports it with Response.Uncompressed. An explicit header, even one naming gzip, turns
that off (see http.Transport.DisableCompression). So the fix is to take the library's
header back off in a RoundTripper before the real transport sees the request.

This lives in its own package rather than in pkg/trp, which is the inbound TRP server
(gin handlers over the database), so that the outbound-only dependency stays free of the
server's imports and can be used from pkg/postman, pkg/web and cmd/fsi alike.
*/
package client

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"time"

	"github.com/trisacrypto/trisa/pkg/openvasp/client"
)

// Timeout matches the timeout on the trisa library's default http client, which
// WithClient below replaces wholesale.
const Timeout = 30 * time.Second

// New returns a TRP client that negotiates and decodes compressed responses correctly.
// It is a drop-in replacement for client.New from the trisa library and takes the same
// options.
func New(opts ...client.ClientOption) (_ *client.Client, err error) {
	// WithClient replaces the whole http.Client, so the defaults the library would
	// otherwise set (30s timeout, a cookie jar for CSRF cookies) are restated here.
	httpClient := &http.Client{
		CheckRedirect: nil,
		Timeout:       Timeout,
	}

	if httpClient.Jar, err = cookiejar.New(nil); err != nil {
		return nil, fmt.Errorf("could not create cookiejar: %w", err)
	}

	// WithClient goes first so that any option the caller passes still wins.
	opts = append([]client.ClientOption{client.WithClient(httpClient)}, opts...)

	var trpc *client.Client
	if trpc, err = client.New(opts...); err != nil {
		return nil, err
	}

	// WithTLSConfig and WithMTLS assign a fresh http.Transport to the client, which
	// would drop the wrapper if it were installed before the options ran. They mutate
	// the http.Client created above, so wrapping whatever transport ended up on it
	// survives those options. It only fails to apply if a caller passes its own
	// WithClient, in which case that client is theirs to configure; none of the call
	// sites in this repo pass any options at all.
	next := httpClient.Transport
	if next == nil {
		// A nil Transport means http.DefaultTransport, and that is what the library's
		// own default client used. Keep sharing it rather than cloning: the call sites
		// build a client per send, and a private transport per send would leave each
		// one's idle connections open until their timeout instead of pooling them.
		next = http.DefaultTransport
	}

	httpClient.Transport = &stripAcceptEncoding{next: next}

	return trpc, nil
}

// stripAcceptEncoding removes the Accept-Encoding header the trisa library hard-codes
// and delegates to the real transport, which then adds "gzip" itself and transparently
// decodes the response. See the package comment for why.
type stripAcceptEncoding struct {
	next http.RoundTripper
}

var _ http.RoundTripper = &stripAcceptEncoding{}

func (t *stripAcceptEncoding) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper must not modify the request it is handed.
	if req.Header.Get("Accept-Encoding") != "" {
		req = req.Clone(req.Context())
		req.Header.Del("Accept-Encoding")
	}

	return t.next.RoundTrip(req)
}
