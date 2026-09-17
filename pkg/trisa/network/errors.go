package network

import "errors"

var (
	ErrNoGRPCPeer             = errors.New("no grpc remote peer info found in context")
	ErrTRISADisabled          = errors.New("trisa network is disabled on this node")
	ErrNoKeyChain             = errors.New("no key chain available on network")
	ErrNoDirectory            = errors.New("no directory configured on the network")
	ErrUnknownPeerCertificate = errors.New("could not verify peer certificate subject info")
	ErrUnknownPeerSubject     = errors.New("could not identify common name on certificate subject")
)
