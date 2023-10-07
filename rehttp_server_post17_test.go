//go:build go1.7
// +build go1.7

package rehttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTransport_RoundTripTimeouts(t *testing.T) {
	// to keep track of any open server requests and ensure the correct number of requests were made
	ch := make(chan bool, 4)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- true
		time.Sleep(time.Millisecond * 100)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	tr := NewTransport(
		http.DefaultTransport,
		RetryAll(RetryMaxRetries(3), RetryAny(
			RetryStatuses(http.StatusTooManyRequests), // retry 429s
			func(attempt Attempt) bool { // retry context deadline exceeded errors
				return attempt.Error != nil && attempt.Error == context.DeadlineExceeded // errors.Is requires go 1.13+
			})),
		ConstDelay(0),
	)
	tr.PerAttemptTimeout = time.Millisecond * 10 // short timeout

	client := http.Client{
		Transport: tr,
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Errorf("error creating request: %s", err)
	}

	_, err = client.Do(req)
	// make sure server has finished and got 4 attempts
	<-ch
	<-ch
	<-ch
	<-ch
	if err == nil {
		t.Error("expected timeout error doing request but got nil")
	}

	// now increase the timeout restriction
	ch = make(chan bool, 4)
	tr.PerAttemptTimeout = time.Second
	res, err := client.Do(req)

	// should have attempted 4 times without going over the timeout
	<-ch
	<-ch
	<-ch
	<-ch

	if err != nil {
		t.Fatalf("got unexpected error doing request: %s", err)
	}
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status code does not match expected: got %d, want %d", res.StatusCode, http.StatusTooManyRequests)
	}

	// now remove the timeout restriction
	ch = make(chan bool, 4)
	tr.PerAttemptTimeout = 0
	res, err = client.Do(req)
	// should have attempted 4 times without going over the timeout
	<-ch
	<-ch
	<-ch
	<-ch

	if err != nil {
		t.Fatalf("got unexpected error doing request: %s", err)
	}
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status code does not match expected: got %d, want %d", res.StatusCode, http.StatusTooManyRequests)
	}
}

func TestTransport_RoundTripOverallTimeout(t *testing.T) {
	ch := make(chan bool, 2)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- true
		time.Sleep(time.Second * 2)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	tr := NewTransport(
		http.DefaultTransport,
		RetryAll(RetryMaxRetries(3), RetryAny(
			RetryStatuses(http.StatusTooManyRequests), // retry 429s
			func(attempt Attempt) bool { // retry context deadline exceeded errors
				return attempt.Error != nil && attempt.Error == context.DeadlineExceeded // errors.Is requires go 1.13+
			})),
		ConstDelay(0),
	)
	tr.PerAttemptTimeout = time.Second

	client := http.Client{
		Transport: tr,
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Errorf("error creating request: %s", err)
	}

	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Millisecond*1500)
	_, err = client.Do(req.WithContext(ctx))
	cancelFunc()
	// should only make 2 attempts
	<-ch
	<-ch
	if err == nil {
		t.Error("expected timeout error doing request but got nil")
	}
}

// TestCancelReader is meant to test that the cancel reader is correctly
// preventing the race-case of being unable to read the body due to a
// preemptively-canceled context.
func TestCancelReader(t *testing.T) {
	rt := NewTransport(http.DefaultTransport, RetryMaxRetries(1), ConstDelay(0))
	rt.PerAttemptTimeout = time.Millisecond * 100
	client := http.Client{
		Transport: rt,
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond * 10)
		w.WriteHeader(http.StatusOK)
		// need a decent number of bytes to make the race case more likely to fail
		// https://github.com/go-kit/kit/issues/773
		_, _ = w.Write(make([]byte, 102400))
	}))
	defer ts.Close()

	ctx := context.Background()

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	res, err := client.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("unexpected error executing request: %s", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Errorf("error reading response body: %s", err)
	}
	if len(b) != 102400 {
		t.Errorf("response byte length does not match expected. got %d, want %d", len(b), 102400)
	}
}
