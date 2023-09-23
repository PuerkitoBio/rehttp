//go:build !go1.13
// +build !go1.13

package rehttp

type timeouter interface {
	Timeout() bool
}

func isTimeoutErr(err error) bool {
	if terr, ok := err.(timeouter); ok {
		return terr.Timeout()
	}
	return false
}
