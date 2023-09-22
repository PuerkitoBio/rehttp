package rehttp

import (
	"errors"
	"net"
	"testing"

	"golang.org/x/net/context"
)

func Test_isTimeoutErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "context timeouts should be retryable",
			err:  context.DeadlineExceeded,
			want: true,
		},
		{
			name: "net op errors are true",
			err: &net.OpError{
				Err: timeoutErr{},
			},
			want: true,
		},
		{
			name: "non-network related errors should not be retryable",
			err:  errors.New("fake error"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTimeoutErr(tt.err); got != tt.want {
				t.Errorf("isTimeoutErr() = %v, want %v", got, tt.want)
			}
		})
	}
}
