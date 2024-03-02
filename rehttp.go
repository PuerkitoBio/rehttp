// Package rehttp implements an HTTP transport that handles retries.
// An HTTP client can be created with a *rehttp.Transport as RoundTripper
// and it will apply the retry strategy to its requests.
//
// The retry strategy is provided by the Transport, which determines
// whether or not the request should be retried, and if so, what delay
// to apply before retrying, based on the RetryFn and DelayFn
// functions passed to NewTransport.
//
// The package offers common delay strategies as ready-made functions that
// return a DelayFn:
//   - ConstDelay(delay time.Duration) DelayFn
//   - ExpJitterDelay(base, max time.Duration) DelayFn
//   - ExpJitterDelayWithRand(base, max time.Duration, generator func(int64) int64) DelayFn
//
// It also provides common retry helpers that return a RetryFn:
//   - RetryIsErr(func(error) bool) RetryFn
//   - RetryHTTPMethods(methods ...string) RetryFn
//   - RetryMaxRetries(max int) RetryFn
//   - RetryStatuses(statuses ...int) RetryFn
//   - RetryStatusInterval(fromStatus, toStatus int) RetryFn
//   - RetryTimeoutErr() RetryFn
//   - RetryTemporaryErr() RetryFn
//
// Those can be combined with RetryAny or RetryAll as needed. RetryAny
// enables retries if any of the RetryFn return true, while RetryAll
// enables retries if all RetryFn return true. Typically, the RetryFn
// of the Transport should use at least RetryMaxRetries and some other
// retry condition(s), combined using RetryAll.
//
// By default, the Transport will buffer the request's body in order to
// be able to retry the request, as a request attempt will consume and
// close the existing body. Sometimes this is not desirable, so it can
// be prevented by setting PreventRetryWithBody to true on the Transport.
// Doing so will disable retries when a request has a non-nil body.
//
// This package requires Go version 1.6+, since it uses the new
// http.Request.Cancel field in order to cancel requests. It doesn't
// implement the deprecated http.Transport.CancelRequest method
// (https://golang.org/pkg/net/http/#Transport.CancelRequest).
//
// On Go1.7+, it uses the context returned by http.Request.Context
// to check for cancelled requests. Before Go1.7, PerAttemptTimeout
// has no effect.
//
// It should work on Go1.5, but only if there is no timeout set on the
// *http.Client. Go's stdlib will return an error on the first request
// if that's the case, because it requires a RoundTripper that
// implements the CancelRequest method.
package rehttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// PRNG is the *math.Rand value to use to add jitter to the backoff
// algorithm used in ExpJitterDelay. By default it uses a *rand.Rand
// initialized with a source based on the current time in nanoseconds.
//
// Deprecated: math/rand sources can panic if used concurrently without
// synchronization. PRNG is no longer used by this package and its use
// outside this package is discouraged.
// https://github.com/PuerkitoBio/rehttp/issues/12
var PRNG = rand.New(rand.NewSource(time.Now().UnixNano()))

// terribly named interface to detect errors that support Temporary.
type temporaryer interface {
	Temporary() bool
}

// Attempt holds the data describing a RoundTrip attempt.
type Attempt struct {
	// Index is the attempt index starting at 0.
	Index int

	// Request is the request for this attempt. If a non-nil Response
	// is present, this is the same as Response.Request, but since a
	// Response may not be available, it is guaranteed to be set on this
	// field.
	Request *http.Request

	// Response is the response for this attempt. It may be nil if an
	// error occurred an no response was received.
	Response *http.Response

	// Error is the error returned by the attempt, if any.
	Error error
}

// retryFn is the signature for functions that implement retry strategies.
type retryFn func(attempt Attempt) (bool, time.Duration)

// DelayFn is the signature for functions that return the delay to apply
// before the next retry.
type DelayFn func(attempt Attempt) time.Duration

// RetryFn is the signature for functions that return whether a
// retry should be done for the request.
type RetryFn func(attempt Attempt) bool

// NewTransport creates a Transport with a retry strategy based on
// retry and delay to control the retry logic. It uses the provided
// RoundTripper to execute the requests. If rt is nil,
// http.DefaultTransport is used.
func NewTransport(rt http.RoundTripper, retry RetryFn, delay DelayFn) *Transport {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &Transport{
		RoundTripper: rt,
		retry:        toRetryFn(retry, delay),
	}
}

// toRetryFn combines retry and delay into a retryFn.
func toRetryFn(retry RetryFn, delay DelayFn) retryFn {
	return func(attempt Attempt) (bool, time.Duration) {
		if ok := retry(attempt); !ok {
			return false, 0
		}
		return true, delay(attempt)
	}
}

// RetryAny returns a RetryFn that allows a retry as long as one of
// the retryFns returns true. If retryFns is empty, it always returns false.
func RetryAny(retryFns ...RetryFn) RetryFn {
	return func(attempt Attempt) bool {
		for _, fn := range retryFns {
			if fn(attempt) {
				return true
			}
		}
		return false
	}
}

// RetryAll returns a RetryFn that allows a retry if all retryFns
// return true. If retryFns is empty, it always returns true.
func RetryAll(retryFns ...RetryFn) RetryFn {
	return func(attempt Attempt) bool {
		for _, fn := range retryFns {
			if !fn(attempt) {
				return false
			}
		}
		return true
	}
}

// RetryMaxRetries returns a RetryFn that retries if the number of
// retries is less than or equal to max.
func RetryMaxRetries(max int) RetryFn {
	return func(attempt Attempt) bool {
		// < instead of <= because attempt.Index is 0-based, so if max = 1
		// (1 retry), it will return true when attempt.Index == 0 only.
		return attempt.Index < max
	}
}

// RetryIsErr returns a RetryFn that retries if the provided
// error predicate - a function that receives an error and returns a
// boolean - returns true for the error associated with the Attempt.
// Note that fn may be called with a nil error.
func RetryIsErr(fn func(error) bool) RetryFn {
	return func(attempt Attempt) bool {
		return fn(attempt.Error)
	}
}

// RetryTemporaryErr returns a RetryFn that retries if the Attempt's error
// is a temporary error. A temporary error is one that implements
// the Temporary() bool method. Most errors from the net package implement
// this.
// This interface was deprecated in go 1.18. Favor RetryTimeoutErr.
// https://github.com/golang/go/issues/45729
func RetryTemporaryErr() RetryFn {
	return RetryIsErr(func(err error) bool {
		if terr, ok := err.(temporaryer); ok {
			return terr.Temporary()
		}
		return false
	})
}

// RetryTimeoutErr returns a RetryFn that retries if the Attempt's error
// is a timeout error. Before go 1.13, a timeout error is one that implements
// the Timeout() bool method. Most errors from the net package implement this.
// After go 1.13, a timeout error is one that implements the net.Error interface
// which includes both Timeout() and Temporary() to make it less likely to
// falsely identify errors that occurred outside of the net package.
func RetryTimeoutErr() RetryFn {
	return RetryIsErr(isTimeoutErr)
}

// RetryStatusInterval returns a RetryFn that retries if the response's
// status code is in the provided half-closed interval [fromStatus, toStatus)
// (that is, it retries if fromStatus <= Response.StatusCode < toStatus, so
// RetryStatusInterval(400, 500) would retry for any 4xx code, but not for
// 500).
func RetryStatusInterval(fromStatus, toStatus int) RetryFn {
	return func(attempt Attempt) bool {
		return attempt.Response != nil &&
			attempt.Response.StatusCode >= fromStatus &&
			attempt.Response.StatusCode < toStatus
	}
}

// RetryStatuses returns a RetryFn that retries if the response's status
// code is one of the provided statuses.
func RetryStatuses(statuses ...int) RetryFn {
	return func(attempt Attempt) bool {
		if attempt.Response == nil {
			return false
		}
		for _, st := range statuses {
			if st == attempt.Response.StatusCode {
				return true
			}
		}
		return false
	}
}

// RetryHTTPMethods returns a RetryFn that retries if the request's
// HTTP method is one of the provided methods. It is meant to be used
// in conjunction with another RetryFn such as RetryTimeoutErr combined
// using RetryAll, otherwise this function will retry any successful
// request made with one of the provided methods.
func RetryHTTPMethods(methods ...string) RetryFn {
	for i, m := range methods {
		methods[i] = strings.ToUpper(m)
	}

	return func(attempt Attempt) bool {
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

// ExpJitterDelay is identical to [ExpJitterDelayWithRand], using
// math/rand.Int63n as the random generator function.
// This package does not call [rand.Seed], so it is the caller's
// responsibility to ensure the default generator is properly seeded.
func ExpJitterDelay(base, max time.Duration) DelayFn {
	return ExpJitterDelayWithRand(base, max, rand.Int63n)
}

// ExpJitterDelayWithRand returns a DelayFn that returns a delay
// between 0 and base * 2^attempt capped at max (an exponential
// backoff delay with jitter). The generator argument is expected
// to generate a random int64 in the half open interval [0, n).
// It is the caller's responsibility to ensure that the function is
// safe for concurrent use.
//
// See the full jitter algorithm in:
// http://www.awsarchitectureblog.com/2015/03/backoff.html
func ExpJitterDelayWithRand(base, max time.Duration, generator func(n int64) int64) DelayFn {
	return func(attempt Attempt) time.Duration {
		exp := math.Pow(2, float64(attempt.Index))
		top := float64(base) * exp
		return time.Duration(
			generator(int64(math.Min(float64(max), top))),
		)
	}
}

// Transport wraps a RoundTripper such as *http.Transport and adds
// retry logic.
type Transport struct {
	http.RoundTripper

	// PreventRetryWithBody prevents retrying if the request has a body. Since
	// the body is consumed on a request attempt, in order to retry a request
	// with a body, the body has to be buffered in memory. Setting this
	// to true avoids this buffering: the retry logic is bypassed if the body
	// is non-nil.
	PreventRetryWithBody bool

	// PerAttemptTimeout can be optionally set to add per-attempt timeouts.
	// These may be used in place of or in conjunction with overall timeouts.
	// For example, a per-attempt timeout of 5s would mean an attempt will
	// be canceled after 5s, then the delay fn will be consulted before
	// potentially making another attempt, which will again be capped at 5s.
	// This means that the overall duration may be up to
	// (PerAttemptTimeout + delay) * n, where n is the maximum attempts.
	// If using an overall timeout (whether on the http client or the request
	// context), the request will stop at whichever timeout is reached first.
	// Your RetryFn can determine if a request hit the per-attempt timeout by
	// checking if attempt.Error == context.DeadlineExceeded (or use errors.Is
	// on go 1.13+).
	// time.Duration(0) signals that no per-attempt timeout should be used.
	// Note that before go 1.7 this option has no effect.
	PerAttemptTimeout time.Duration

	// retry is a function that determines if the request should be retried.
	// Unless a retry is prevented based on PreventRetryWithBody, all requests
	// go through that function, even those that are typically considered
	// successful.
	//
	// If it returns false, no retry is attempted, otherwise a retry is
	// attempted after the specified duration.
	retry retryFn
}

// RoundTrip implements http.RoundTripper for the Transport type.
// It calls its underlying http.RoundTripper to execute the request, and
// adds retry logic as per its configuration.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var attempt int
	preventRetry := req.Body != nil && req.Body != http.NoBody && t.PreventRetryWithBody

	// used as a baseline to set fresh timeouts per-attempt if needed
	ctx := getRequestContext(req)

	// get the done cancellation channel for the context, will be nil
	// for < go1.7.
	done := contextForRequest(req)

	// buffer the body if needed
	var br *bytes.Reader
	if req.Body != nil && req.Body != http.NoBody && !preventRetry {
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
		var cancel context.CancelFunc = func() {} // empty unless a timeout is set
		reqWithTimeout := req
		if t.PerAttemptTimeout != 0 {
			reqWithTimeout, cancel = getPerAttemptTimeoutInfo(ctx, req, t.PerAttemptTimeout)
		}
		res, err := t.RoundTripper.RoundTrip(reqWithTimeout)
		if preventRetry {
			return injectCancelReader(res, cancel), err
		}

		retry, delay := t.retry(Attempt{
			Request:  reqWithTimeout,
			Response: res,
			Index:    attempt,
			Error:    err,
		})
		if !retry {
			return injectCancelReader(res, cancel), err
		}

		if br != nil {
			// Per Go's doc: "RoundTrip should not modify the request,
			// except for consuming and closing the Body", so the only thing
			// to reset on the request is the body, if any.
			if _, serr := br.Seek(0, 0); serr != nil {
				// failed to retry, return the results
				return injectCancelReader(res, cancel), err
			}
			reqWithTimeout.Body = ioutil.NopCloser(br)
		}
		// close the disposed response's body, if any
		if res != nil {
			_, _ = io.Copy(ioutil.Discard, res.Body)
			res.Body.Close()
		}
		cancel() // we're done with this response and won't be returning it, so it's safe to cancel immediately

		select {
		case <-time.After(delay):
			attempt++
		case <-done:
			// request canceled by caller (post-1.7), don't retry
			return nil, errors.New("net/http: request canceled")
		case <-req.Cancel:
			// request canceled by caller (pre-1.7), don't retry
			return nil, errors.New("net/http: request canceled")
		}
	}
}
