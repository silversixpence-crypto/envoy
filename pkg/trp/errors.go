package trp

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/trisacrypto/envoy/pkg/web/api/v1"
)

var (
	ErrMethodNotAllowed         = errors.New(http.StatusText(http.StatusMethodNotAllowed))
	ErrMissingRequestIdentifier = errors.New("missing request identifier in header")
	ErrSupportedVersions        = fmt.Errorf("unsupported API version; this server supports %s", SupportedAPIVersions)
	ErrMalformedContentType     = errors.New("malformed content-type header")
	ErrUnsupportedContentType   = errors.New("content-type header must be application/json")
	ErrWebhookUnavailable       = errors.New("envoy compliance callback is not available")
)

// The comment sent to a counterparty when the compliance callback rejects a transfer
// without saying why; TRP has no machine readable rejection codes.
const defaultRejection = "the counterparty rejected this transfer"

// Returns a not found JSON response
func (s *Server) NotFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, api.NotFound)
}

// Returns a method not allowed JSON response
func (s *Server) NotAllowed(c *gin.Context) {
	c.JSON(http.StatusMethodNotAllowed, api.NotAllowed)
}
