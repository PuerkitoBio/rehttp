// Package rehttp implements an HTTP transport that handles retries.
//
// An HTTP Client can be created with a *rehttp.Transport as Transport
// and it will apply the retry strategy to its requests, e.g.:
//
//     client := &http.Client{
//       Transport: rehttp.NewTransport(
//         nil,                            // will use http.DefaultTransport
//         rehttp.RetryTemporaryErr(3),    // max 3 retries
//         rehttp.ConstDelay(time.Second), // wait 1s between retries
//       ),
//       Timeout: 30 * time.Second, // timeout applies to all retries as a whole
//     }
//
// The retry strategy is provided by the Transport.Retry field, which holds
// a function that returns whether or not the request should be retried,
// and if so, what delay to apply before retrying.
//
// For convenience, ToRetryFn combines two functions - a ShouldRetryFn
// that returns whether or not the request should be retried, and a DelayFn
// that returns the delay to apply - into a RetryFn that can be used as
// value for Transport.Retry. As seen in the example above, NewTransport
// also accepts the retry strategy as a pair of ShouldRetryFn and DelayFn.
//
// The package offers common delay strategies as ready-made functions that
// return a DelayFn:
//     - ConstDelay(delay time.Duration) DelayFn
//     - NoDelay() DelayFn
//     - ExponentialDelay(initialDelay time.Duration) DelayFn
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
// will disable retries when a request has a non-nil body, the RetryFn
// won't get called in those cases.
//
package rehttp

import (
	"bytes"
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

// RetryFn is the signature for functions that implement retry strategies.
// The attempt parameter is the attempt number corresponding to the response
// and error received, starting at 0.
type RetryFn func(req *http.Request, res *http.Response, attempt int, err error) (bool, time.Duration)

// DelayFn is the signature for functions that return the delay to apply
// before the next retry. The attempt parameter is the attempt number
// corresponding to the response and error received, starting at 0.
type DelayFn func(req *http.Request, res *http.Response, attempt int, err error) time.Duration

// ShouldRetryFn is the signature for functions that return whether a
// retry should be done for the request. The attempt parameter is the
// attempt number corresponding to the response and error received,
// starting at 0.
type ShouldRetryFn func(req *http.Request, res *http.Response, attempt int, err error) bool

// NewTransport creates a Transport with a retry strategy based on
// shouldRetry and delay to control the retry logic. It uses the provided
// CancelRoundTripper to execute the requests. If rt is nil,
// http.DefaultTransport is used, but the package panics if it isn't set
// to a CancelRoundTripper (which it is by default).
func NewTransport(rt CancelRoundTripper, shouldRetry ShouldRetryFn, delay DelayFn) *Transport {
	if rt == nil {
		rt = http.DefaultTransport.(CancelRoundTripper)
	}
	return &Transport{
		rt:    rt,
		Retry: ToRetryFn(shouldRetry, delay),
	}
}

// ToRetryFn combines shouldRetry and delay into a RetryFn that can be used
// as Transport.Retry value.
func ToRetryFn(shouldRetry ShouldRetryFn, delay DelayFn) RetryFn {
	return func(req *http.Request, res *http.Response, attempt int, err error) (bool, time.Duration) {
		retry := shouldRetry(req, res, attempt, err)
		if !retry {
			return false, 0
		}
		return true, delay(req, res, attempt, err)
	}
}

// RetryAny returns a ShouldRetryFn that allows a retry as long as one of
// the retryFns returns true.
func RetryAny(retryFns ...ShouldRetryFn) ShouldRetryFn {
	return func(req *http.Request, res *http.Response, attempt int, err error) bool {
		for _, fn := range retryFns {
			if fn(req, res, attempt, err) {
				return true
			}
		}
		return false
	}
}

// RetryAll returns a ShouldRetryFn that allows a retry if all retryFns
// return true.
func RetryAll(retryFns ...ShouldRetryFn) ShouldRetryFn {
	return func(req *http.Request, res *http.Response, attempt int, err error) bool {
		for _, fn := range retryFns {
			if !fn(req, res, attempt, err) {
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
	return func(req *http.Request, res *http.Response, attempt int, err error) bool {
		if attempt >= maxRetries {
			return false
		}
		if terr, ok := err.(temporaryer); ok {
			return terr.Temporary()
		}
		return false
	}
}

// RetryStatus500 returns a ShouldRetryFn that retries up to maxRetries times
// for a status code 5xx.
func RetryStatus500(maxRetries int) ShouldRetryFn {
	return func(req *http.Request, res *http.Response, attempt int, err error) bool {
		if attempt >= maxRetries {
			return false
		}
		return res != nil && res.StatusCode >= 500 && res.StatusCode < 600 // who knows
	}
}

// RetryHTTPMethods returns a ShouldRetryFn that retries up to maxRetries
// times if the request's HTTP method is one of the provided methods.
func RetryHTTPMethods(maxRetries int, methods ...string) ShouldRetryFn {
	for i, m := range methods {
		methods[i] = strings.ToUpper(m)
	}

	return func(req *http.Request, res *http.Response, attempt int, err error) bool {
		if attempt >= maxRetries {
			return false
		}
		curMeth := strings.ToUpper(req.Method)
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
	return func(req *http.Request, res *http.Response, attempt int, err error) time.Duration {
		return delay
	}
}

// NoDelay returns a DelayFn that always returns 0.
func NoDelay() DelayFn {
	return func(req *http.Request, res *http.Response, attempt int, err error) time.Duration {
		return 0
	}
}

// ExponentialDelay returns a DelayFn that returns an exponential delay based
// on initialDelay at the power of attempt + 1 (so the first delay is
// initialDelay, then initialDelay ** 2, etc.).
func ExponentialDelay(initialDelay time.Duration) DelayFn {
	return func(req *http.Request, res *http.Response, attempt int, err error) time.Duration {
		return time.Duration(math.Pow(float64(initialDelay), float64(attempt+1)))
	}
}

// LinearDelay returns a DelayFn that returns a delay that grows linearly
// based on the number of attempts.
func LinearDelay(initialDelay time.Duration) DelayFn {
	return func(req *http.Request, res *http.Response, attempt int, err error) time.Duration {
		return time.Duration(attempt+1) * initialDelay
	}
}

// Transport wraps a CancelRoundTripper such as *http.Transport and adds
// retry logic.
type Transport struct {
	rt CancelRoundTripper

	// PreventRetryWithBody prevents retrying if the request has a body. Since
	// the body is consumed on a request attempt, in order to retry a request
	// with a body, the body has to be buffered in memory. Setting this
	// to true avoids this buffering, the retry logic is bypassed if a body
	// is present.
	PreventRetryWithBody bool

	// Retry is a function that determines if the request should be retried.
	// Unless a retry is prevented based on PreventRetryWithBody, all requests
	// go through that function, even those that are typically considered
	// successful.
	//
	// If it returns false, no retry is attempted, otherwise a retry is
	// attempted after the specified duration.
	Retry RetryFn
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
		res, err := t.rt.RoundTrip(req)
		if preventRetry {
			return res, err
		}

		retry, delay := t.Retry(req, res, attempt, err)
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
		case <-req.Cancel:
			return res, err
		}
	}
}
