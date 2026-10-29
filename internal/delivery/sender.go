package delivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// UserAgent is sent with every delivery.
const UserAgent = "Hookd/1"

// maxResponseBytes is how much of a response body is read before the
// connection is released. The body is thrown away.
const maxResponseBytes = 64 << 10

// Sender POSTs signed deliveries to endpoints. It never follows redirects:
// a 3xx answer is returned like any other status (R18).
type Sender struct {
	client  *http.Client
	timeout time.Duration
}

// NewSender returns a Sender whose requests, body included, must finish
// within timeout (HOOKD_DELIVERY_TIMEOUT, R16).
func NewSender(timeout time.Duration) *Sender {
	return &Sender{
		timeout: timeout,
		client: &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Send POSTs body to rawURL as JSON with the given extra headers (the
// signature headers) and returns the response status. err is set when no
// response was received, for example on a timeout or a refused connection;
// its text is meant to be stored with the delivery.
func (s *Sender) Send(ctx context.Context, rawURL string, header http.Header, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, s.describe(err)
	}
	defer resp.Body.Close()
	// The status decides the outcome; a body that fails to read changes
	// nothing.
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	return resp.StatusCode, nil
}

// describe turns a client error into the text stored with the delivery. It
// drops the `Post "url":` prefix, which repeats the endpoint URL.
func (s *Sender) describe(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if uerr.Timeout() {
			return fmt.Errorf("timeout: no response within %v", s.timeout)
		}
		return uerr.Err
	}
	return err
}
