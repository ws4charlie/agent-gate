package x402

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Challenge is a parsed 402.
type Challenge struct {
	Required PaymentRequired
}

// Pick returns the first requirement this client can pay: exact, eip3009.
func (c Challenge) Pick() (Requirements, error) {
	for _, r := range c.Required.Accepts {
		if r.Scheme == SchemeExact && (r.Extra.AssetTransferMethod == "" || r.Extra.AssetTransferMethod == "eip3009") {
			return r, nil
		}
	}
	return Requirements{}, errors.New("x402: no supported payment option")
}

// Fetcher does the HTTP half of the client: receive a 402, then retry with a
// payment header. It holds no keys; signing is the wallet's job (I10).
type Fetcher struct{ HTTP *http.Client }

func (f Fetcher) client() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return http.DefaultClient
}

func (f Fetcher) do(ctx context.Context, method, url string, body []byte, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return f.client().Do(req)
}

// Probe makes the first, unpaid request. A 402 yields a Challenge; any other
// answer is returned as-is (the resource was free or failed).
func (f Fetcher) Probe(ctx context.Context, method, url string, body []byte) (*Challenge, *http.Response, error) {
	resp, err := f.do(ctx, method, url, body, nil)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		return nil, resp, nil
	}
	ch, err := ChallengeFrom(resp)
	if err != nil {
		return nil, nil, err
	}
	return ch, nil, nil
}

// ChallengeFrom parses a 402 response (and closes its body). The header is the
// only part used; the body is data from outside and is discarded.
func ChallengeFrom(resp *http.Response) (*Challenge, error) {
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	h := resp.Header.Get(HeaderRequired)
	if h == "" {
		return nil, errors.New("x402: 402 without PAYMENT-REQUIRED header")
	}
	var pr PaymentRequired
	if err := DecodeHeader(h, &pr); err != nil {
		return nil, err
	}
	if pr.X402Version != Version {
		return nil, fmt.Errorf("x402: unsupported version %d", pr.X402Version)
	}
	return &Challenge{Required: pr}, nil
}

// Pay retries the request with the signed payload. The caller reads the body
// and the PAYMENT-RESPONSE header.
func (f Fetcher) Pay(ctx context.Context, method, url string, body []byte, p PaymentPayload) (*http.Response, error) {
	h, err := EncodeHeader(p)
	if err != nil {
		return nil, err
	}
	return f.do(ctx, method, url, body, map[string]string{HeaderSignature: h})
}
