//go:build go1.7
// +build go1.7

package rehttp

import (
	"context"
	"net/http"
	"time"
)

func getRequestContext(req *http.Request) context.Context {
	return req.Context()
}

func getPerAttemptTimeoutInfo(ctx context.Context, req *http.Request, timeout time.Duration) (*http.Request, context.CancelFunc) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	return req.WithContext(tctx), cancel
}
