package bank

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Fake is an in-memory Rail for tests. Nothing here talks to a network.
type Fake struct {
	mu       sync.Mutex
	n        int
	byKey    map[string]string // idempotency key -> request id
	requests map[string]*Request
	txns     map[string]*Txn // by request id
	// Sent counts RequestSend calls that created a request (a repeat of a key does not count).
	Sent int
	// Calls counts every RequestSend call, repeats included.
	Calls int
	// SendErr, when set, is returned by RequestSend (ErrRejected or a transport error).
	SendErr error
}

func NewFake() *Fake {
	return &Fake{byKey: map[string]string{}, requests: map[string]*Request{}, txns: map[string]*Txn{}}
}

func (f *Fake) RequestSend(_ context.Context, r SendReq) (Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.SendErr != nil {
		return Request{}, f.SendErr
	}
	if id, ok := f.byKey[r.IdempotencyKey]; ok {
		return *f.requests[id], nil
	}
	f.n++
	id := fmt.Sprintf("req-%d", f.n)
	f.byKey[r.IdempotencyKey] = id
	f.requests[id] = &Request{ID: id, Status: ReqPending}
	f.Sent++
	return *f.requests[id], nil
}

func (f *Fake) GetRequest(_ context.Context, id string) (Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.requests[id]
	if !ok {
		return Request{}, errors.New("fake: no such request")
	}
	return *r, nil
}

func (f *Fake) TxnForRequest(_ context.Context, id string) (*Txn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.txns[id]
	if !ok {
		return nil, nil
	}
	c := *t
	return &c, nil
}

// SetRequest sets the status (and reviews) the bank reports for a request.
func (f *Fake) SetRequest(id, status string, reviews ...Review) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.requests[id]
	r.Status, r.Reviews = status, reviews
}

// SetTxn sets the transaction the bank reports for a request.
func (f *Fake) SetTxn(requestID, status string, posted bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txns[requestID] = &Txn{ID: "txn-" + requestID, Status: status, Posted: posted}
}
