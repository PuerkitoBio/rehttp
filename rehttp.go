// Package rehttp implements an HTTP transport that handles retries.
//
// An HTTP Client can be created with a *rehttp.Transport as RoundTripper
// and it will apply the retry strategy to its requests, e.g.:
//
//     tr, err := rehttp.NewTransport(
//         nil,                            // will use http.DefaultTransport
//         rehttp.RetryTemporaryErr(3),    // max 3 retries
//         rehttp.ConstDelay(time.Second), // wait 1s between retries
//     )
//     if err != nil {
//       // handle err
//     }
//     client := &http.Client{
//       Transport: tr,
//       Timeout: 30 * time.Second, // timeout applies to all retries as a whole
//     }
//
// The retry strategy is provided by the Transport, which holds
// a function that returns whether or not the request should be retried,
// and if so, what delay to apply before retrying, based on the ShouldRetryFn
// and DelayFn functions passed to NewTransport.
//
// The package offers common delay strategies as ready-made functions that
// return a DelayFn:
//     - ConstDelay(delay time.Duration) DelayFn
//     - NoDelay() DelayFn
//     - ExponentialDelay(initialDelay, base time.Duration) DelayFn
//     - LinearDelay(initialDelay time.Duration) DelayFn
//
// It also provides common retry predicates that return a ShouldRetryFn:
//     - RetryTemporaryErr(maxRetries int) ShouldRetryFn
//     - RetryStatus500(maxRetries int) ShouldRetryFn
//     - RetryHTTPMethods(maxRetries int, methods ...string) ShouldRetryFn
//
// Those can be combined with RetryAny or RetryAll as needed.
//
// The Transport will buffer the request's body in order to be able to
// retry the request, as a request attempt will consume and close the
// existing body. Sometimes this is not desirable, so it can be prevented
// by setting PreventRetryWithBody to true on the Transport. Doing so
// will disable retries when a request has a non-nil body.
//
package rehttp

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"strings"
	"time"
)

// terribly named interface to detect errors that support Temporary.
type temporaryer interface {
	Temporary() bool
}

// CancelRoundTripper is a RoundTripper that supports CancelRequest.
// The *http.Transport type implements this interface.
type CancelRoundTripper interface {
	http.RoundTripper
	CancelRequest(*http.Request)
}

// Attempt holds the data for a RoundTrip attempt. The Index field is the
// attempt number corresponding to the Response and Error received, starting
// at 0.
type Attempt struct {
	Index    int
	Request  *http.Request
	Response *http.Response
	Error    error
}

// retryFn is the signature for functions that implement retry strategies.
type retryFn func(attempt Attempt) (bool, time.Duration)

// DelayFn is the signature for functions that return the delay to apply
// before the next retry.
type DelayFn func(attempt Attempt) time.Duration

// ShouldRetryFn is the signature for functions that return whether a
// retry should be done for the request.
type ShouldRetryFn func(attempt Attempt) bool

// NewTransport creates a Transport with a retry strategy based on
// shouldRetry and delay to control the retry logic. It uses the provided
// CancelRoundTripper to execute the requests. If rt is nil,
// http.DefaultTransport is used. An error is returned if http.DefaultTransport
// is not a CancelRoundTripper (which it is by default).
func NewTransport(rt CancelRoundTripper, shouldRetry ShouldRetryFn, delay DelayFn) (*Transport, error) {
	if rt == nil {
		var ok bool
		rt, ok = http.DefaultTransport.(CancelRoundTripper)
		if !ok {
			return nil, errors.New("http.DefaultTransport is not a CancelRoundTripper")
		}
	}
	return &Transport{
		CancelRoundTripper: rt,
		retry:              toRetryFn(shouldRetry, delay),
	}, nil
}

// toRetryFn combines shouldRetry and delay into a retryFn.
func toRetryFn(shouldRetry ShouldRetryFn, delay DelayFn) retryFn {
	return func(attempt Attempt) (bool, time.Duration) {
		retry := shouldRetry(attempt)
		if !retry {
			return false, 0
		}
		return true, delay(attempt)
	}
}

// RetryAny returns a ShouldRetryFn that allows a retry as long as one of
// the retryFns returns true. If retryFns is empty, it always returns false.
func RetryAny(retryFns ...ShouldRetryFn) ShouldRetryFn {
	return func(attempt Attempt) bool {
		for _, fn := range retryFns {
			if fn(attempt) {
				return true
			}
		}
		return false
	}
}

// RetryAll returns a ShouldRetryFn that allows a retry if all retryFns
// return true. If retryFns is empty, it always returns true.
func RetryAll(retryFns ...ShouldRetryFn) ShouldRetryFn {
	return func(attempt Attempt) bool {
		for _, fn := range retryFns {
			if !fn(attempt) {
				return false
			}
		}
		return true
	}
}

// RetryTemporaryErr returns a ShouldRetryFn that retries up to maxRetries
// times for a temporary error. A temporary error is one that implements
// the Temporary() bool method. Most errors from the net package implement
// this.
func RetryTemporaryErr(maxRetries int) ShouldRetryFn {
	return func(attempt Attempt) bool {
		if attempt.Index >= maxRetries {
			return false
		}
		if terr, ok := attempt.Error.(temporaryer); ok {
			return terr.Temporary()
		}
		return false
	}
}

// RetryStatus500 returns a ShouldRetryFn that retries up to maxRetries times
// for a status code 5xx.
func RetryStatus500(maxRetries int) ShouldRetryFn {
	return func(attempt Attempt) bool {
		if attempt.Index >= maxRetries {
			return false
		}
		return attempt.Response != nil &&
			attempt.Response.StatusCode >= 500 &&
			attempt.Response.StatusCode < 600 // who knows
	}
}

// RetryHTTPMethods returns a ShouldRetryFn that retries up to maxRetries
// times if the request's HTTP method is one of the provided methods.
func RetryHTTPMethods(maxRetries int, methods ...string) ShouldRetryFn {
	for i, m := range methods {
		methods[i] = strings.ToUpper(m)
	}

	return func(attempt Attempt) bool {
		if attempt.Index >= maxRetries {
			return false
		}
		curMeth := strings.ToUpper(attempt.Request.Method)
		for _, m := range methods {
			if curMeth == m {
				return true
			}
		}
		return false
	}
}

// ConstDelay returns a DelayFn that always returns the same delay.
func ConstDelay(delay time.Duration) DelayFn {
	return func(attempt Attempt) time.Duration {
		return delay
	}
}

// NoDelay returns a DelayFn that always returns 0.
func NoDelay() DelayFn {
	return func(attempt Attempt) time.Duration {
		return 0
	}
}

// ExponentialDelay returns a DelayFn that returns an exponential delay from
// the initialDelay (in the provided base, e.g. an initialDelay of 2s in base
// time.Second will give delays of 2s, 4s, 8s...). There is no special case
// handling, so passing 1s in a time.Second base will yield 1s every time.
func ExponentialDelay(initialDelay, base time.Duration) DelayFn {
	inBase := float64(initialDelay) / float64(base)
	return func(attempt Attempt) time.Duration {
		newVal := math.Pow(inBase, float64(attempt.Index+1))
		return time.Duration(newVal * float64(base))
	}
}

// LinearDelay returns a DelayFn that returns a delay that grows linearly
// based on the number of attempts.
func LinearDelay(initialDelay time.Duration) DelayFn {
	return func(attempt Attempt) time.Duration {
		return time.Duration(attempt.Index+1) * initialDelay
	}
}

// Transport wraps a CancelRoundTripper such as *http.Transport and adds
// retry logic.
type Transport struct {
	CancelRoundTripper

	// PreventRetryWithBody prevents retrying if the request has a body. Since
	// the body is consumed on a request attempt, in order to retry a request
	// with a body, the body has to be buffered in memory. Setting this
	// to true avoids this buffering, the retry logic is bypassed if a body
	// is present.
	PreventRetryWithBody bool

	// retry is a function that determines if the request should be retried.
	// Unless a retry is prevented based on PreventRetryWithBody, all requests
	// go through that function, even those that are typically considered
	// successful.
	//
	// If it returns false, no retry is attempted, otherwise a retry is
	// attempted after the specified duration.
	retry retryFn
}

// Per Go's doc: "CancelRequest should only be called after RoundTrip
// has returned."
//
// So it should not have an impact on this Transport (it doesn't return before
// the retries are done).

// RoundTrip implements http.RoundTripper for the Transport type.
// It calls its underlying RoundTripper to execute the request, and
// adds retry logic as per its configuration.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var attempt int
	preventRetry := req.Body != nil && t.PreventRetryWithBody

	// buffer the body if needed
	var br *bytes.Reader
	if req.Body != nil && !preventRetry {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, req.Body); err != nil {
			// cannot even try the first attempt, body has been consumed
			req.Body.Close()
			return nil, err
		}
		req.Body.Close()

		br = bytes.NewReader(buf.Bytes())
		req.Body = ioutil.NopCloser(br)
	}

	for {
		res, err := t.CancelRoundTripper.RoundTrip(req)
		if preventRetry {
			return res, err
		}

		retry, delay := t.retry(Attempt{
			Request:  req,
			Response: res,
			Index:    attempt,
			Error:    err,
		})
		if !retry {
			return res, err
		}

		if br != nil {
			// Per Go's doc: "RoundTrip should not modify the request,
			// except for consuming and closing the Body", so the only thing
			// to reset on the request is the body, if any.
			if _, serr := br.Seek(0, 0); serr != nil {
				// failed to retry, return the results
				return res, err
			}
			req.Body = ioutil.NopCloser(br)
		}
		select {
		case <-time.After(delay):
			attempt++
			// close the disposed response's body, if any
			if res != nil {
				res.Body.Close()
			}
		case <-req.Cancel:
			// request canceled, don't retry
			return res, err
		}
	}
}
