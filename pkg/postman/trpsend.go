package postman

// Outgoing Travel Rule Protocol support.
//
// Upstream Envoy (all releases up to and including v1.4.0) implements the INBOUND
// half of TRP only: pkg/trp handles incoming inquiries, but pkg/web/send.go returns
// "TRP sending is temporarily disabled as we refresh Envoy to v1.0.0" and
// Packet.TRP() returns nil. This file adds the outbound half that the TRP-only demo
// needs: converting a TRISA payload into a TRP v3 inquiry, recording the
// counterparty's resolution as the incoming message, and sealing locally generated
// messages with the node's own storage key (TRP is plaintext on the wire).

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/trisacrypto/trisa/pkg/ivms101"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
	"github.com/trisacrypto/trisa/pkg/slip0044"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	generic "github.com/trisacrypto/trisa/pkg/trisa/data/generic/v1beta1"
	"github.com/trisacrypto/trisa/pkg/trisa/envelope"
	"github.com/trisacrypto/trisa/pkg/trisa/keys"
)

var (
	ErrNoTRPPayload     = errors.New("cannot create a trp message without a payload")
	ErrNoTRPIdentity    = errors.New("cannot create a trp message without an ivms101 identity payload")
	ErrNoTRPTransaction = errors.New("cannot create a trp message without a transaction payload")
)

// InquiryFromPayload converts a TRISA payload into a TRP v3 inquiry. It is the
// inverse of PayloadFromInquiry: payloads created by the web API prepare endpoint
// carry a generic.Transaction, while payloads that arrived over TRP carry a
// generic.TRP message.
func InquiryFromPayload(payload *trisa.Payload) (inquiry *trp.Inquiry, err error) {
	if payload == nil {
		return nil, ErrNoTRPPayload
	}

	if payload.Identity == nil {
		return nil, ErrNoTRPIdentity
	}

	inquiry = &trp.Inquiry{
		IVMS101: &ivms101.IdentityPayload{},
	}

	if err = payload.Identity.UnmarshalTo(inquiry.IVMS101); err != nil {
		return nil, fmt.Errorf("could not unmarshal ivms101 identity payload: %w", err)
	}

	scrubIdentity(inquiry.IVMS101)

	if payload.Transaction == nil {
		return nil, ErrNoTRPTransaction
	}

	// The common case: a transaction prepared by the local API or UI. The prepared
	// amount is in display units and travels in base units.
	txn := &generic.Transaction{}

	if err = payload.Transaction.UnmarshalTo(txn); err == nil {
		inquiry.Asset = AssetFromNetwork(txn.Network, txn.AssetType)
		inquiry.Amount = ToBaseUnits(txn.Amount, inquiry.Asset)

		return inquiry, nil
	}

	// Otherwise this payload was created from a TRP message on the wire.
	msg := &generic.TRP{}

	if err = payload.Transaction.UnmarshalTo(msg); err != nil {
		return nil, fmt.Errorf("could not unmarshal transaction payload: %w", err)
	}

	if in := msg.GetInquiry(); in != nil {
		inquiry.Amount = in.Amount
		inquiry.Callback = in.Callback
		inquiry.Asset = assetFromMap(in.Asset)
	}

	if txn := msg.GetTransaction(); txn != nil {
		if inquiry.Amount == 0 {
			inquiry.Amount = txn.Amount
		}

		if inquiry.Asset == nil {
			inquiry.Asset = AssetFromNetwork(txn.Network, txn.AssetType)
		}
	}

	return inquiry, nil
}

// scrubIdentity drops set-but-empty IVMS101 sub-messages before an identity payload
// goes out over TRP. Envoy's prepare endpoint populates dateAndPlaceOfBirth and
// nationalIdentification even when they carry no data, and the protobuf marshaller
// then emits them as {} — which strict TRP parsers (21 Analytics' trpd) reject with
// "did not match any variant of untagged enum OneToN". The TRP 3.2 spec examples
// omit these members entirely.
func scrubIdentity(identity *ivms101.IdentityPayload) {
	if identity == nil {
		return
	}

	// 21 Analytics' IVMS101 model accepts only originator, beneficiary and
	// originatingVASP — the beneficiary VASP is the receiver's own identity — and
	// unknown fields are rejected outright.
	identity.BeneficiaryVasp = nil
	identity.TransferPath = nil
	identity.PayloadMetadata = nil

	persons := append(
		identity.GetOriginator().GetOriginatorPersons(),
		identity.GetBeneficiary().GetBeneficiaryPersons()...,
	)

	for _, person := range persons {
		natural := person.GetNaturalPerson()

		if natural == nil {
			continue
		}

		if dob := natural.DateAndPlaceOfBirth; dob != nil && dob.DateOfBirth == "" && dob.PlaceOfBirth == "" {
			natural.DateAndPlaceOfBirth = nil
		}

		if nid := natural.NationalIdentification; nid != nil && nid.NationalIdentifier == "" {
			natural.NationalIdentification = nil
		}
	}
}

// dtiRegistry maps common tickers onto their ISO 24165 Digital Token Identifiers
// and the token's decimals. Strict TRP implementations (21 Analytics' trpd) reject
// plain tickers in the dti field, and TRP 3.2 sends amounts as integers in the
// asset's base units (a nonzero u128 on their side) — so BTC 0.001 must travel as
// 100000 sats. Extend as networks are added.
var dtiRegistry = map[string]struct {
	DTI      string
	Decimals int
}{
	"BTC": {"4H95J0R2X", 8},
}

// dtiDecimals is the reverse index: decimals by DTI, for inbound conversion.
var dtiDecimals = func() map[string]int {
	m := make(map[string]int, len(dtiRegistry))

	for _, entry := range dtiRegistry {
		m[entry.DTI] = entry.Decimals
	}

	return m
}()

// ToBaseUnits converts a display amount (0.001 BTC) into the asset's integer base
// units (100000 sats) when the asset's DTI is registered; other assets pass through
// unchanged for permissive counterparties.
func ToBaseUnits(amount float64, asset *trp.Asset) float64 {
	if asset != nil {
		if decimals, ok := dtiDecimals[asset.DTI]; ok {
			return math.Round(amount * math.Pow10(decimals))
		}
	}

	return amount
}

// FromBaseUnits restores the display amount from integer base units on inbound
// messages whose asset DTI is registered.
func FromBaseUnits(amount float64, dti string) float64 {
	if decimals, ok := dtiDecimals[dti]; ok {
		return amount / math.Pow10(decimals)
	}

	return amount
}

// AssetFromNetwork maps a network and/or asset type (e.g. "BTC", "ETH") onto a TRP
// asset. Registered DTIs are preferred, then SLIP-0044 coin types; anything else is
// passed through in the DTI field so the counterparty still learns what was
// transferred (permissive receivers accept it; strict ones reject it loudly).
func AssetFromNetwork(network, assetType string) *trp.Asset {
	for _, candidate := range []string{network, assetType} {
		if entry, ok := dtiRegistry[strings.ToUpper(strings.TrimSpace(candidate))]; ok {
			return &trp.Asset{DTI: entry.DTI}
		}
	}

	for _, candidate := range []string{network, assetType} {
		if candidate == "" {
			continue
		}

		if coin, err := slip0044.ParseCoinType(candidate); err == nil {
			asset := &trp.Asset{SLIP044: coin}

			// SLIP-0044 registers bitcoin as coin type 0, which trp.Asset.Validate()
			// cannot tell apart from "unset" — carry the symbol in DTI as well so
			// the asset validates and the counterparty still learns the network.
			if coin == 0 {
				asset.DTI = strings.ToUpper(strings.TrimSpace(candidate))
			}

			return asset
		}
	}

	switch {
	case network != "":
		return &trp.Asset{DTI: network}
	case assetType != "":
		return &trp.Asset{DTI: assetType}
	default:
		return nil
	}
}

// assetFromMap rebuilds a TRP asset from the map stored on a generic.TRPInquiry.
func assetFromMap(asset map[string]string) *trp.Asset {
	if len(asset) == 0 {
		return nil
	}

	if slip, ok := asset["slip044"]; ok && slip != "" {
		if coin, err := slip0044.ParseCoinType(slip); err == nil {
			return &trp.Asset{SLIP044: coin}
		}
	}

	if dti, ok := asset["dti"]; ok && dti != "" {
		return &trp.Asset{DTI: dti}
	}

	return nil
}

// BeneficiaryAddress returns the crypto address of the beneficiary on an envelope,
// which TRP requires in the address field of an approval resolution.
func BeneficiaryAddress(env *envelope.Envelope) string {
	if env == nil {
		return ""
	}

	payload, err := env.Payload()

	if err != nil || payload == nil {
		return ""
	}

	txn := &generic.Transaction{}

	if err = payload.Transaction.UnmarshalTo(txn); err == nil && txn.Beneficiary != "" {
		return txn.Beneficiary
	}

	identity := &ivms101.IdentityPayload{}

	if err = payload.Identity.UnmarshalTo(identity); err == nil {
		return FindAccount(identity.Beneficiary)
	}

	return ""
}

// ReceiveResolution records the counterparty's reply to an outgoing TRP inquiry as
// the incoming message of the packet. TRP replies are bare JSON rather than secure
// envelopes, so the incoming envelope is synthesised from the outgoing payload with
// the transfer state that the resolution implies.
func (p *TRPPacket) ReceiveResolution(in *trp.Resolution) (err error) {
	transferState := trisa.TransferPending

	switch {
	case in == nil:
		transferState = trisa.TransferPending
	case in.Rejected != "":
		transferState = trisa.TransferRejected
	case in.Approved != nil:
		transferState = trisa.TransferAccepted
	}

	p.message = in

	opts := []envelope.Option{
		envelope.WithEnvelopeID(p.envelopeID.String()),
		envelope.WithTransferState(transferState),
	}

	if p.In.Envelope, err = envelope.New(p.payload, opts...); err != nil {
		return fmt.Errorf("could not create incoming trp resolution envelope: %w", err)
	}

	return nil
}

// EchoIncoming synthesises the incoming half of a packet from its outgoing message.
// TRP acknowledges resolutions and confirmations with an empty 204 response, so the
// local node echoes its own message back to keep the envelope history and the
// transaction status consistent with the way TRISA transfers are recorded.
func (p *Packet) EchoIncoming() (err error) {
	if p.Out == nil || p.Out.Envelope == nil {
		return ErrNoTRPPayload
	}

	if p.Out.Envelope.IsError() {
		if p.In.Envelope, err = envelope.WrapError(p.Out.Envelope.Error(), envelope.WithEnvelopeID(p.Out.Envelope.ID())); err != nil {
			return fmt.Errorf("could not echo outgoing trp rejection: %w", err)
		}

		return nil
	}

	var payload *trisa.Payload

	if payload, err = p.Out.Envelope.Payload(); err != nil {
		return fmt.Errorf("could not read outgoing payload: %w", err)
	}

	opts := []envelope.Option{
		envelope.WithEnvelopeID(p.Out.Envelope.ID()),
		envelope.WithTransferState(p.Out.Envelope.TransferState()),
	}

	if p.In.Envelope, err = envelope.New(payload, opts...); err != nil {
		return fmt.Errorf("could not echo outgoing trp message: %w", err)
	}

	return nil
}

// SealLocal encrypts and seals both halves of a packet with the node's own storage
// key. Protocols that are plaintext on the wire (TRP) still have to store their
// envelopes encrypted at rest, and there is no counterparty key to seal against.
func (p *Packet) SealLocal(storageKey keys.PublicKey) (err error) {
	if storageKey == nil {
		return ErrNoSealingKey
	}

	if p.Out != nil && p.Out.Envelope != nil && !p.Out.Envelope.IsError() {
		p.Out.StorageKey = storageKey
		p.Out.SealingKey = storageKey

		if _, err = p.Out.Seal(); err != nil {
			return fmt.Errorf("could not seal outgoing trp envelope: %w", err)
		}
	}

	if p.In != nil && p.In.Envelope != nil {
		if !p.In.Envelope.IsError() {
			if p.In.Envelope, _, err = p.In.Envelope.Encrypt(); err != nil {
				return fmt.Errorf("could not encrypt incoming trp envelope: %w", err)
			}

			if p.In.Envelope, _, err = p.In.Envelope.Seal(envelope.WithSealingKey(storageKey)); err != nil {
				return fmt.Errorf("could not seal incoming trp envelope: %w", err)
			}
		}

		p.In.original = p.In.Envelope.Proto()
	}

	return nil
}

// RevealIncoming swaps the sealed incoming envelope back for its cleartext form once
// the secure envelope models have been persisted, so that API handlers can still
// render the payload of the reply to the caller.
func (p *Packet) RevealIncoming(clear *envelope.Envelope) {
	if clear != nil && p.In != nil {
		p.In.Envelope = clear
	}
}
