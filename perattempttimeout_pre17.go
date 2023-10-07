//go:build !go1.7
// +build !go1.7

package rehttp

import (
	"context"
	"net/http"
	"time"
)

func getRequestContext(req *http.Request) context.Context {
	return nil // req.Context() doesn't exist before 1.7
}

func getPerAttemptTimeoutInfo(ctx context.Context, req *http.Request, timeout time.Duration) (*http.Request, context.CancelFunc) {
	// req.WithContext() doesn't exist before 1.7, so noop
	return req, func() {}
}
