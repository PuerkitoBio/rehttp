package rehttp

import (
	"context"
	"io"
	"net/http"
)

type cancelReader struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (r cancelReader) Close() error {
	r.cancel()
	return r.ReadCloser.Close()
}

// injectCancelReader propagates the ability for the caller to cancel the request context
// once done with the response. If the transport cancels before the body stream is read,
// a race begins where the caller may be unable to read the response bytes before the stream
// is closed and an error is returned. This helper function wraps a response body in a
// io.ReadCloser that cancels the context once the body is closed, preventing a context leak.
// Solution based on https://github.com/go-kit/kit/issues/773.
func injectCancelReader(res *http.Response, cancel context.CancelFunc) *http.Response {
	if res == nil {
		return nil
	}

	res.Body = cancelReader{
		ReadCloser: res.Body,
		cancel:     cancel,
	}
	return res
}
