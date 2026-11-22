// Package bank is the bank rail: an agent asks Gate to pay a bank recipient by
// ACH, Gate decides first, and the payment goes to the bank as a request that a
// human approves there. Gate tracks it through every state and writes each
// step to an append-only chain.
//
// The bank is behind the Rail interface. Mercury is the one implementation
// (sandbox only for now); Fake is the in-memory one for tests.
package bank

import (
	"context"
	"errors"
	"time"
)

// Request statuses of a bank payment request (Mercury's request-send-money).
const (
	ReqPending   = "pendingApproval"
	ReqApproved  = "approved"
	ReqRejected  = "rejected"
	ReqCancelled = "cancelled"
)

// Transaction statuses.
const (
	TxnPending   = "pending"
	TxnSent      = "sent"
	TxnCancelled = "cancelled"
	TxnFailed    = "failed"
	TxnReversed  = "reversed"
	TxnBlocked   = "blocked"
)

// Review is one approval decision recorded at the bank.
type Review struct {
	Reviewer string `json:"reviewer"`
	Status   string `json:"status"`
	At       string `json:"at,omitempty"`
}

// Request is the bank's view of a payment request.
type Request struct {
	ID      string
	Status  string // one of the Req* values; anything else is unknown
	Reviews []Review
}

// Txn is the bank's view of the money movement a request turns into.
type Txn struct {
	ID     string
	Status string // one of the Txn* values; anything else is unknown
	Posted bool   // the bank has posted it
}

// SendReq asks the bank for a payment that waits for approval.
type SendReq struct {
	RecipientID string
	Cents       int64
	// IdempotencyKey is passed through: the bank collapses a repeat of the same
	// key into one request, so a retry after a lost answer cannot pay twice.
	IdempotencyKey string
	Note           string
}

// ErrRejected means the bank refused the request itself (a 4xx): sending it
// again will not help.
var ErrRejected = errors.New("bank: request rejected")

// Rail is the part of a bank Gate needs.
type Rail interface {
	// RequestSend files a payment request. It never moves money by itself.
	RequestSend(ctx context.Context, r SendReq) (Request, error)
	GetRequest(ctx context.Context, id string) (Request, error)
	// TxnForRequest returns the transaction an approved request created, or
	// nil when there is none yet.
	TxnForRequest(ctx context.Context, requestID string) (*Txn, error)
}

// State is where a bank payment is.
type State string

const (
	Refused              State = "refused"
	AwaitingGateApproval State = "awaiting_gate_approval"
	AwaitingBankApproval State = "awaiting_bank_approval"
	BankApproved         State = "bank_approved"
	Sent                 State = "sent"
	Settled              State = "settled"
	Cancelled            State = "cancelled"
	Failed               State = "failed"
	Returned             State = "returned"
)

// Final reports whether no further change is expected. Settled is not final:
// an ACH can still come back for ReturnWindow.
func (s State) Final() bool {
	switch s {
	case Refused, Cancelled, Failed, Returned:
		return true
	}
	return false
}

// ReturnWindow is how long Gate keeps watching a sent payment for an ACH return.
const ReturnWindow = 5 * 24 * time.Hour

// order lists the forward path of a payment that goes through.
var order = []State{AwaitingBankApproval, BankApproved, Sent, Settled}

func rank(s State) int {
	for i, o := range order {
		if o == s {
			return i
		}
	}
	return -1
}

// allowed reports whether from -> to is a legal single step.
func allowed(from, to State) bool {
	switch from {
	case AwaitingGateApproval:
		return to == AwaitingBankApproval || to == Refused
	case AwaitingBankApproval:
		return to == BankApproved || to == Cancelled || to == Failed
	case BankApproved:
		return to == Sent || to == Cancelled || to == Failed
	case Sent:
		return to == Settled || to == Returned
	case Settled:
		return to == Returned
	}
	return false
}
