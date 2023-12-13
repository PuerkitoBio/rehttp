package rehttp

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockRoundTripper struct {
	t *testing.T

	mu     sync.Mutex
	calls  int
	bodies []string
	retFn  func(int, *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()

	att := m.calls
	m.calls++
	if req.Body != nil {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, req.Body)
		req.Body.Close()
		require.Nil(m.t, err)
		m.bodies = append(m.bodies, buf.String())
	}
	m.mu.Unlock()

	return m.retFn(att, req)
}

func (m *mockRoundTripper) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mockRoundTripper) Bodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bodies
}

func TestMockClientRetry(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	tr := NewTransport(mock, RetryAll(RetryMaxRetries(1), RetryTemporaryErr()), ConstDelay(0))

	client := &http.Client{
		Transport: tr,
	}
	_, err := client.Get("http://example.com")
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, tempErr{}, uerr.Err)
	}
	assert.Equal(t, 2, mock.Calls())
}

func TestMockClientFailBufferBody(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	tr := NewTransport(mock, RetryAll(RetryMaxRetries(1), RetryTemporaryErr()), ConstDelay(0))

	client := &http.Client{
		Transport: tr,
	}
	_, err := client.Post("http://example.com", "text/plain", iotest.TimeoutReader(strings.NewReader("hello")))
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, iotest.ErrTimeout, uerr.Err)
	}
	assert.Equal(t, 0, mock.Calls())
}

func TestMockClientPreventRetryWithBody(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	tr := NewTransport(mock, RetryAll(RetryMaxRetries(1), RetryTemporaryErr()), ConstDelay(0))
	tr.PreventRetryWithBody = true

	client := &http.Client{
		Transport: tr,
	}

	_, err := client.Post("http://example.com", "text/plain", strings.NewReader("test"))
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, tempErr{}, uerr.Err)
	}
	assert.Equal(t, 1, mock.Calls()) // did not retry
	assert.Equal(t, []string{"test"}, mock.Bodies())
}

func TestMockClientRetryWithBody(t *testing.T) {
	newRequest := func(body io.Reader) *http.Request {
		req, err := http.NewRequest("POST", "http://example.com", body)
		assert.NoError(t, err)
		req.Header.Set("Content-Type", "text/plain")
		return req
	}

	tests := []struct {
		name        string
		req         *http.Request
		retFn       func(att int, req *http.Request) (*http.Response, error)
		retries     int
		retriesBody []string
		err         error
	}{
		{
			name: "temp-error",
			req:  newRequest(strings.NewReader("hello")),
			retFn: func(att int, req *http.Request) (*http.Response, error) {
				return nil, tempErr{}
			},
			err:         tempErr{},
			retries:     4,
			retriesBody: []string{"hello", "hello", "hello", "hello"},
		},
		{
			name: "307-redirect",
			req:  newRequest(strings.NewReader("hello")),
			retFn: func(att int, req *http.Request) (*http.Response, error) {
				if att >= 1 {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return &http.Response{StatusCode: 307, Body: io.NopCloser(strings.NewReader(""))}, nil
			},
			retries:     4,
			retriesBody: []string{"hello", "hello", "hello", "hello"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockRoundTripper{t: t, retFn: tt.retFn}

			tr := NewTransport(mock, RetryAny(RetryMaxRetries(3), RetryStatusInterval(300, 500)), ConstDelay(0))

			client := &http.Client{
				Transport: tr,
			}
			_, err := client.Do(tt.req)
			assert.ErrorIs(t, err, tt.err)
			assert.Equal(t, tt.retries, mock.Calls())
			assert.Equal(t, tt.retriesBody, mock.Bodies())
		})
	}
}

func TestMockClientRetryTimeout(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, &net.OpError{
			Err: timeoutErr{},
		}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	tr := NewTransport(mock, RetryAll(RetryMaxRetries(1), RetryTimeoutErr()), ConstDelay(0))

	client := &http.Client{
		Transport: tr,
	}
	_, err := client.Get("http://example.com")
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, &net.OpError{
			Err: timeoutErr{},
		}, uerr.Err)
	}
	assert.Equal(t, 2, mock.Calls())
}
