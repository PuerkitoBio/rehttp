package rehttp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNoDelay(t *testing.T) {
	fn := NoDelay()
	want := time.Duration(0)
	for i := 0; i < 5; i++ {
		delay := fn(nil, nil, i, nil)
		assert.Equal(t, want, delay, "%d", i)
	}
}

func TestConstDelay(t *testing.T) {
	want := 2 * time.Second
	fn := ConstDelay(want)
	for i := 0; i < 5; i++ {
		delay := fn(nil, nil, i, nil)
		assert.Equal(t, want, delay, "%d", i)
	}
}

func TestLinearDelay(t *testing.T) {
	initial := 2 * time.Second
	fn := LinearDelay(initial)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second, 10 * time.Second}
	for i := 0; i < len(want); i++ {
		got := fn(nil, nil, i, nil)
		assert.Equal(t, want[i], got, "%d", i)
	}
}

func TestExponentialDelay(t *testing.T) {
	initial := 2 * time.Second
	fn := ExponentialDelay(initial, time.Second)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second}
	for i := 0; i < len(want); i++ {
		got := fn(nil, nil, i, nil)
		assert.Equal(t, want[i], got, "%d", i)
	}

	initial = 100 * time.Millisecond
	fn = ExponentialDelay(initial, 10*time.Millisecond)
	want = []time.Duration{100 * time.Millisecond, time.Second, 10 * time.Second}
	for i := 0; i < len(want); i++ {
		got := fn(nil, nil, i, nil)
		assert.Equal(t, want[i], got, "%d", i)
	}
}
