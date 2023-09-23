//go:build go1.13
// +build go1.13

package rehttp

import (
	"errors"
	"net"
)

func isTimeoutErr(err error) bool {
	var neterr net.Error
	if errors.As(err, &neterr) && neterr.Timeout() {
		return true
	}

	return false
}
