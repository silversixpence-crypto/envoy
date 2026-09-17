package postman

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/trisa/pkg/ivms101"
	"github.com/trisacrypto/trisa/pkg/openvasp/trp/v3"
	api "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	generic "github.com/trisacrypto/trisa/pkg/trisa/data/generic/v1beta1"
	"google.golang.org/protobuf/types/known/anypb"
)

func TransactionFromPayload(in *api.Payload) *models.Transaction {
	var (
		err                error
		originator         string
		originatorAddress  string
		beneficiary        string
		beneficiaryAddress string
		virtualAsset       string
		amount             float64
	)

	data := &generic.Transaction{}
	err = in.Transaction.UnmarshalTo(data)

	if err != nil {
		// TRP payloads carry the reference transaction inside a generic.TRP message.
		trpmsg := &generic.TRP{}
		if terr := in.Transaction.UnmarshalTo(trpmsg); terr == nil && trpmsg.Transaction != nil {
			data = trpmsg.Transaction
			err = nil

			// TRP 3.2 amounts travel as integers in the asset's base units;
			// restore display units for storage and the UI.
			if inq := trpmsg.GetInquiry(); inq != nil && inq.Asset != nil {
				data.Amount = FromBaseUnits(data.Amount, inq.Asset["dti"])
			}
		}
	}

	if err == nil {
		switch {
		case data.Network != "" && data.AssetType != "":
			virtualAsset = fmt.Sprintf("%s (%s)", data.Network, data.AssetType)
		case data.Network != "":
			virtualAsset = data.Network
		case data.AssetType != "":
			virtualAsset = data.AssetType
		}

		amount = data.Amount
		originatorAddress = data.Originator
		beneficiaryAddress = data.Beneficiary
	}

	identity := &ivms101.IdentityPayload{}
	if err = in.Identity.UnmarshalTo(identity); err == nil {
		if identity.Originator != nil {
			originator = FindName(identity.Originator.OriginatorPersons...)
		}

		if identity.Beneficiary != nil {
			beneficiary = FindName(identity.Beneficiary.BeneficiaryPersons...)
		}

		if originatorAddress == "" {
			originatorAddress = FindAccount(identity.Originator)
		}

		if beneficiaryAddress == "" {
			beneficiaryAddress = FindAccount(identity.Beneficiary)
		}
	}

	return &models.Transaction{
		Originator:         sql.NullString{Valid: originator != "", String: originator},
		OriginatorAddress:  sql.NullString{Valid: originatorAddress != "", String: originatorAddress},
		Beneficiary:        sql.NullString{Valid: beneficiary != "", String: beneficiary},
		BeneficiaryAddress: sql.NullString{Valid: beneficiaryAddress != "", String: beneficiaryAddress},
		VirtualAsset:       virtualAsset,
		Amount:             amount,
	}
}

func FindName(persons ...*ivms101.Person) (name string) {
	// Search all persons for the first legal name available. Use the last available
	// non-zero name for any other name identifier types.
	for _, person := range persons {
		switch t := person.Person.(type) {
		case *ivms101.Person_LegalPerson:
			if t.LegalPerson.Name != nil {
				for _, identifier := range t.LegalPerson.Name.NameIdentifiers {
					// Set the name found to the current legal person name
					if identifier.LegalPersonName != "" {
						name = identifier.LegalPersonName

						// If this is the legal name, short circuit and return it.
						if identifier.LegalPersonNameIdentifierType == ivms101.LegalPersonLegal {
							return name
						}
					}
				}
			}
		case *ivms101.Person_NaturalPerson:
			if t.NaturalPerson.Name != nil {
				for _, identifier := range t.NaturalPerson.Name.NameIdentifiers {
					// Set the name found to the current natural person name
					if identifier.PrimaryIdentifier != "" {
						name = strings.TrimSpace(fmt.Sprintf("%s %s", identifier.SecondaryIdentifier, identifier.PrimaryIdentifier))

						// If this is the legal name of the person, short circuit and return it.
						if identifier.NameIdentifierType == ivms101.NaturalPersonLegal {
							return name
						}
					}
				}
			}
		}

	}

	// Return whatever non-zero name we found, or empty string if we found nothing.
	return name
}

func FindAccount(person any) (account string) {
	if person == nil {
		return ""
	}

	switch t := person.(type) {
	case *ivms101.Originator:
		for _, account = range t.AccountNumbers {
			if account != "" {
				return account
			}
		}
	case *ivms101.Beneficiary:
		for _, account = range t.AccountNumbers {
			if account != "" {
				return account
			}
		}
	}

	// Return whatever non-zero account we found, or empty string if we found nothing.
	return account
}

//===========================================================================
// TRP Payloads
//===========================================================================

func PayloadFromInquiry(inquiry *trp.Inquiry) (payload *api.Payload, err error) {
	payload = &api.Payload{
		SentAt: time.Now().Format(time.RFC3339), // The TRP inquiry is the first message, so sent at is now.
	}

	if payload.Identity, err = anypb.New(inquiry.IVMS101); err != nil {
		return nil, err
	}

	var network string
	asset := make(map[string]string)
	if inquiry.Asset != nil {
		if inquiry.Asset.DTI != "" {
			asset["dti"] = inquiry.Asset.DTI
			network = inquiry.Asset.DTI
		}
		if inquiry.Asset.SLIP044 != 0 {
			asset["slip044"] = inquiry.Asset.SLIP044.Symbol()
			network = inquiry.Asset.SLIP044.Symbol()
		}
	}

	transaction := &generic.TRP{
		EnvelopeId: inquiry.Info.RequestIdentifier,
		Headers: &generic.TRPInfo{
			Version:           inquiry.Info.APIVersion,
			RequestIdentifier: inquiry.Info.RequestIdentifier,
			Extensions:        inquiry.Info.APIExtensions,
		},
		Message: &generic.TRP_Inquiry{
			Inquiry: &generic.TRPInquiry{
				Asset:    asset,
				Amount:   inquiry.Amount,
				Callback: inquiry.Callback,
			},
		},
		Transaction: &generic.Transaction{
			Originator:  FindAccount(inquiry.IVMS101.Originator),
			Beneficiary: FindAccount(inquiry.IVMS101.Beneficiary),
			Amount:      inquiry.Amount,
			Network:     network,
		},
	}

	if len(inquiry.Extensions) > 0 {
		var extensions []byte
		if extensions, err = json.Marshal(inquiry.Extensions); err == nil {
			transaction.Extensions = string(extensions)
		}
	}

	if payload.Transaction, err = anypb.New(transaction); err != nil {
		return nil, err
	}

	return payload, nil
}

// PayloadFromResolution builds the payload that records a TRP resolution (an approval or
// a rejection) as a secure envelope. TRP replies are bare JSON on the wire, so the
// identity and the reference transaction are carried over from the payload of the
// inquiry this resolution answers and the decision itself is stored as the TRP message.
//
// Storing the approval verbatim matters beyond the audit trail: the confirmation this
// node later sends has to go to the callback the beneficiary named in its approval, and
// this envelope is the only place that URL is recorded.
func PayloadFromResolution(base *api.Payload, res *trp.Resolution) (payload *api.Payload, err error) {
	if base == nil {
		return nil, ErrNoTRPPayload
	}

	msg := &generic.TRP{}

	switch {
	case res == nil:
		return nil, ErrNoTRPResolution
	case res.Rejected != "":
		msg.Message = &generic.TRP_Rejected{
			Rejected: &generic.TRPRejected{Rejected: res.Rejected},
		}
	case res.Approved != nil:
		msg.Message = &generic.TRP_Approved{
			Approved: &generic.TRPApproved{
				Address:  res.Approved.Address,
				Callback: res.Approved.Callback,
			},
		}
	default:
		// A version-only resolution is an acknowledgement rather than a decision;
		// there is nothing to record beyond the transaction status.
		return nil, ErrNoTRPResolution
	}

	copyTRPContext(base, msg)

	payload = &api.Payload{
		Identity:   base.Identity,
		SentAt:     base.SentAt,
		ReceivedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if payload.Transaction, err = anypb.New(msg); err != nil {
		return nil, err
	}

	return payload, nil
}

// PayloadFromConfirmation builds the payload that records a TRP confirmation (the
// originator reporting the on-chain transaction id, or canceling the transfer) as a
// secure envelope, carrying the identity and reference transaction over from the payload
// of the message it follows.
func PayloadFromConfirmation(base *api.Payload, in *trp.Confirmation) (payload *api.Payload, err error) {
	if base == nil {
		return nil, ErrNoTRPPayload
	}

	msg := &generic.TRP{}

	switch {
	case in == nil:
		return nil, ErrNoTRPConfirmation
	case in.TXID != "":
		msg.Message = &generic.TRP_Confirmed{
			Confirmed: &generic.TRPConfirmed{Txid: in.TXID},
		}
	case in.Canceled != "":
		msg.Message = &generic.TRP_Canceled{
			Canceled: &generic.TRPCanceled{Canceled: in.Canceled},
		}
	default:
		return nil, ErrNoTRPConfirmation
	}

	copyTRPContext(base, msg)

	// A confirmed transaction has an on-chain identifier that the reference transaction
	// did not have when the inquiry was made; record it where the UI reads it from.
	if in.TXID != "" && msg.Transaction != nil {
		msg.Transaction.Txid = in.TXID
	}

	payload = &api.Payload{
		Identity:   base.Identity,
		SentAt:     base.SentAt,
		ReceivedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if payload.Transaction, err = anypb.New(msg); err != nil {
		return nil, err
	}

	return payload, nil
}

// copyTRPContext carries the TRP headers and the reference transaction of an earlier
// message in the transfer onto a new TRP message. The base payload is either a TRP
// message (this transfer arrived over TRP) or a plain transaction (this node prepared
// the inquiry through the web API), and both shapes are handled.
func copyTRPContext(base *api.Payload, msg *generic.TRP) {
	if base.Transaction == nil {
		return
	}

	prev := &generic.TRP{}

	if err := base.Transaction.UnmarshalTo(prev); err == nil {
		msg.EnvelopeId = prev.EnvelopeId
		msg.Headers = prev.Headers
		msg.Extensions = prev.Extensions
		msg.Transaction = prev.Transaction

		return
	}

	txn := &generic.Transaction{}

	if err := base.Transaction.UnmarshalTo(txn); err == nil {
		msg.Transaction = txn
	}
}
