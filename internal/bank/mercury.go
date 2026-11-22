package bank

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	mercury "github.com/MercuryTechnologies/mercury-go"
	"github.com/MercuryTechnologies/mercury-go/option"
)

// SandboxBaseURL is Mercury's sandbox API. The rail is sandbox-only for now.
const SandboxBaseURL = "https://api-sandbox.mercury.com/api/v1/"

// IsSandbox reports whether base points at Mercury's sandbox. Anything that
// simulates bank behaviour (a return) is refused unless this is true.
func IsSandbox(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	return err == nil && u.Scheme == "https" && u.Hostname() == "api-sandbox.mercury.com"
}

// MercuryConfig configures the Mercury rail.
type MercuryConfig struct {
	BaseURL   string // default SandboxBaseURL
	AccountID string // the checking account payments are paid from
	Token     string // API token; read from the environment, never logged
	Proxy     string // optional egress proxy URL (production wants a fixed IP); empty = direct
}

// Mercury is the Mercury Rail. It uses the official Go SDK: the SDK has
// request-send-money (Payments.Request/Get/GetTransactions), so no raw calls.
type Mercury struct {
	c       mercury.Client
	account string
}

// NewMercury builds the rail. The token is held by the SDK client only.
func NewMercury(cfg MercuryConfig) (*Mercury, error) {
	if cfg.Token == "" {
		return nil, errors.New("bank: Mercury token is empty")
	}
	if cfg.AccountID == "" {
		return nil, errors.New("bank: Mercury account id is empty")
	}
	base := cfg.BaseURL
	if base == "" {
		base = SandboxBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil || pu.Host == "" {
			return nil, errors.New("bank: AG_MERCURY_PROXY is not a URL")
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	hc := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	c := mercury.NewClient(option.WithBaseURL(base), option.WithAPIKey(cfg.Token), option.WithHTTPClient(hc))
	return &Mercury{c: c, account: cfg.AccountID}, nil
}

func fromSDK(a *mercury.SendMoneyApproval) Request {
	r := Request{ID: a.RequestID, Status: string(a.Status)}
	for _, v := range a.Reviews {
		r.Reviews = append(r.Reviews, Review{Reviewer: v.ReviewerUserID, Status: string(v.Status), At: v.ReviewedAt})
	}
	return r
}

// classify turns a 4xx from the bank into ErrRejected.
func classify(err error) error {
	var ae *mercury.Error
	if errors.As(err, &ae) && ae.StatusCode >= 400 && ae.StatusCode < 500 && ae.StatusCode != 408 && ae.StatusCode != 429 {
		return fmt.Errorf("%w: http %d", ErrRejected, ae.StatusCode)
	}
	return err
}

func (m *Mercury) RequestSend(ctx context.Context, r SendReq) (Request, error) {
	p := mercury.PaymentRequestParams{
		Amount:         float64(r.Cents) / 100,
		IdempotencyKey: r.IdempotencyKey,
		PaymentMethod:  mercury.SendMoneyPaymentMethodACH,
		RecipientID:    r.RecipientID,
	}
	if r.Note != "" {
		p.Note = mercury.String(r.Note)
	}
	res, err := m.c.Payments.Request(ctx, m.account, p)
	if err != nil {
		return Request{}, classify(err)
	}
	return fromSDK(res), nil
}

func (m *Mercury) GetRequest(ctx context.Context, id string) (Request, error) {
	res, err := m.c.Payments.Get(ctx, id)
	if err != nil {
		return Request{}, classify(err)
	}
	return fromSDK(res), nil
}

func (m *Mercury) TxnForRequest(ctx context.Context, requestID string) (*Txn, error) {
	res, err := m.c.Payments.GetTransactions(ctx, m.account, mercury.PaymentGetTransactionsParams{
		RequestID: mercury.String(requestID),
	})
	if err != nil {
		return nil, classify(err)
	}
	if len(res.Transactions) == 0 {
		return nil, nil
	}
	t := res.Transactions[0]
	return &Txn{ID: t.ID, Status: string(t.Status), Posted: t.PostedAt != ""}, nil
}
