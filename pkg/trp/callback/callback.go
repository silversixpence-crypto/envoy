/*
Package callback derives and verifies the per-transfer capability tokens that this node
embeds in every TRP callback URL it hands out.

TRP callbacks are unauthenticated by design: the counterparty simply POSTs to the URL it
was given. Without a token, anyone who learns (or guesses) a transfer's envelope id can
approve or reject an outbound transfer, or confirm an inbound one, because the envelope
id is the only thing the URL contains. Binding a keyed token to the envelope id and to
the purpose of the callback means a caller has to have been handed the URL by this node.

The package deliberately depends on nothing else in Envoy so that both the web server
(which builds callbacks) and the TRP server (which verifies them) can import it.
*/
package callback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
)

const (
	// PurposeResolve tokens authorize a counterparty to resolve an outbound inquiry.
	PurposeResolve = "resolve"

	// PurposeConfirm tokens authorize an originator to confirm an approved transfer.
	PurposeConfirm = "confirm"

	transfersPath = "/transfers"
)

// Token derives the capability token for a transfer and callback purpose. The token is
// the base64url (unpadded) HMAC-SHA256 of "<envelope id>:<purpose>"; it is stable for
// the lifetime of the key so it does not have to be stored alongside the transfer.
func Token(key []byte, envelopeID, purpose string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(envelopeID + ":" + purpose))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify reports whether the supplied token authorizes the given transfer and purpose.
// The comparison is constant time, and a missing key or token never verifies so that a
// node without a configured key cannot be tricked into accepting an empty token.
func Verify(key []byte, envelopeID, purpose, token string) bool {
	if len(key) == 0 || token == "" {
		return false
	}

	return hmac.Equal([]byte(Token(key, envelopeID, purpose)), []byte(token))
}

// CallbackURL builds the URL a counterparty should post a resolution or confirmation
// for this transfer to:
//
//	<scheme>://<host>[/<prefix>]/transfers/<envelope id>/<purpose>/<token>
//
// endpoint is the configured TRP endpoint, which may carry a scheme (stripped) and a
// path prefix (kept, so a path routing proxy in front of a single public hostname can
// find this node again in multi-tenant deployments).
//
// When no key is configured the legacy tokenless URL is produced; such a node can only
// accept the callback it generates if it also allows unauthenticated callbacks.
func CallbackURL(endpoint, scheme, envelopeID, purpose string, key []byte) string {
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	host, prefix, _ := strings.Cut(endpoint, "/")

	uri := &url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   transfersPath + "/" + envelopeID + "/" + purpose,
	}

	if len(key) > 0 {
		uri.Path += "/" + Token(key, envelopeID, purpose)
	}

	if prefix != "" {
		uri.Path = "/" + strings.TrimSuffix(prefix, "/") + uri.Path
	}

	return uri.String()
}
