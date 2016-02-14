package rehttp

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/aybabtme/iocontrol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
	"golang.org/x/net/context/ctxhttp"
)

type mockRoundTripper struct {
	t *testing.T

	mu     sync.Mutex
	calls  int
	bodies []string
	retFn  func(int, *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()

	att := m.calls
	m.calls++
	if req.Body != nil {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, req.Body)
		req.Body.Close()
		require.Nil(m.t, err)
		m.bodies = append(m.bodies, buf.String())
	}
	m.mu.Unlock()

	return m.retFn(att, req)
}

func (m *mockRoundTripper) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mockRoundTripper) Bodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bodies
}

func assertNetTimeoutErr(t *testing.T, err error) {
	if assert.NotNil(t, err) {
		nerr, ok := err.(net.Error)
		require.True(t, ok)
		assert.True(t, nerr.Timeout())
		t.Logf("%#v", err)
	}
}

func assertURLTimeoutErr(t *testing.T, err error) {
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		nerr, ok := uerr.Err.(net.Error)
		require.True(t, ok)
		assert.True(t, nerr.Timeout())
		t.Logf("%#v", nerr)
	}
}

func TestContextCancelOnRetry(t *testing.T) {
	callCnt := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnt := atomic.AddInt32(&callCnt, 1)
		switch cnt {
		case 1:
			w.WriteHeader(500)
		default:
			time.Sleep(2 * time.Second)
			fmt.Fprint(w, r.URL.Path)
		}
	}))
	defer srv.Close()

	// cancel while waiting on retry response
	tr := NewTransport(nil, RetryAll(RetryMaxAttempts(1), RetryStatusRange(500, 600)), ConstDelay(0))
	c := &http.Client{
		Transport: tr,
	}

	ctx, cancelFn := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancelFn()

	res, err := ctxhttp.Get(ctx, c, srv.URL+"/test")
	require.Nil(t, res)

	assert.Equal(t, context.DeadlineExceeded, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&callCnt))

	// cancel while waiting on delay
	atomic.StoreInt32(&callCnt, 0)
	tr = NewTransport(nil, RetryAll(RetryMaxAttempts(1), RetryStatusRange(500, 600)), ConstDelay(2*time.Second))
	c = &http.Client{
		Transport: tr,
	}

	ctx, cancelFn = context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancelFn()

	res, err = ctxhttp.Get(ctx, c, srv.URL+"/test")
	require.Nil(t, res)

	assert.Equal(t, context.DeadlineExceeded, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&callCnt))
}

func TestContextCancel(t *testing.T) {
	// server that doesn't reply before the timeout
	wg := sync.WaitGroup{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, r.URL.Path)
		wg.Done()
	}))
	defer srv.Close()

	tr := NewTransport(nil, RetryAll(RetryMaxAttempts(1), RetryStatusRange(500, 600)), ConstDelay(0))
	c := &http.Client{
		Transport: tr,
	}

	ctx, cancelFn := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancelFn()

	wg.Add(1)
	res, err := ctxhttp.Get(ctx, c, srv.URL+"/test")
	require.Nil(t, res)

	assert.Equal(t, context.DeadlineExceeded, err)
	wg.Wait()
}

func TestClientTimeoutSlowBody(t *testing.T) {
	// server that flushes the headers ASAP, but sends the body slowly
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		tw := iocontrol.ThrottledWriter(w, 2, time.Second)
		fmt.Fprint(tw, r.URL.Path)
	}))
	defer srv.Close()

	runWithClient := func(c *http.Client) {
		res, err := c.Get(srv.URL + "/test")

		// should receive a response
		require.Nil(t, err)
		require.NotNil(t, res)

		// should fail with timeout while reading body
		_, err = io.Copy(ioutil.Discard, res.Body)
		res.Body.Close()
		assertNetTimeoutErr(t, err)
	}

	// test with retry transport
	tr := NewTransport(nil, RetryAll(RetryMaxAttempts(2), RetryTemporaryErr()), ConstDelay(time.Second))

	c := &http.Client{
		Transport: tr,
		Timeout:   time.Second,
	}
	runWithClient(c)

	// test with default transport, make sure it behaves the same way
	c = &http.Client{Timeout: time.Second}
	runWithClient(c)
}

func TestClientTimeoutOnRetry(t *testing.T) {
	// server returns 500 on first call, sleeps 2s before reply on other calls
	callCnt := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnt := atomic.AddInt32(&callCnt, 1)
		switch cnt {
		case 1:
			w.WriteHeader(500)
		default:
			time.Sleep(2 * time.Second)
			fmt.Fprint(w, r.URL.Path)
		}
	}))
	defer srv.Close()

	// timeout while waiting for retry request
	tr := NewTransport(nil, RetryAll(RetryMaxAttempts(1), RetryStatusRange(500, 600)), ConstDelay(0))
	c := &http.Client{
		Transport: tr,
		Timeout:   time.Second,
	}
	res, err := c.Get(srv.URL + "/test")
	require.Nil(t, res)
	assertURLTimeoutErr(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&callCnt))

	atomic.StoreInt32(&callCnt, 0)

	// timeout while waiting for retry delay
	tr = NewTransport(nil, RetryAll(RetryMaxAttempts(1), RetryStatusRange(500, 600)), ConstDelay(2*time.Second))
	c = &http.Client{
		Transport: tr,
		Timeout:   time.Second,
	}
	res, err = c.Get(srv.URL + "/test")
	require.Nil(t, res)
	assertURLTimeoutErr(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&callCnt))
}

func TestClientTimeout(t *testing.T) {
	// server that doesn't reply before the timeout
	wg := sync.WaitGroup{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, r.URL.Path)
		wg.Done()
	}))
	defer srv.Close()

	// test with retry transport
	tr := NewTransport(nil, RetryAll(RetryMaxAttempts(2), RetryTemporaryErr()), ConstDelay(time.Second))
	c := &http.Client{
		Transport: tr,
		Timeout:   time.Second,
	}
	wg.Add(1)
	res, err := c.Get(srv.URL + "/test")
	require.Nil(t, res)
	assertURLTimeoutErr(t, err)

	// test with default transport, make sure it returns the same error
	c = &http.Client{Timeout: time.Second}
	wg.Add(1)
	res, err = c.Get(srv.URL + "/test")
	require.Nil(t, res)
	assertURLTimeoutErr(t, err)

	wg.Wait()
}

func TestTransportTimeout(t *testing.T) {
	// server that doesn't reply before the timeout
	wg := sync.WaitGroup{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, r.URL.Path)
		wg.Done()
	}))
	defer srv.Close()

	// test with retry transport
	httpTr := &http.Transport{
		ResponseHeaderTimeout: time.Second,
	}
	tr := NewTransport(httpTr, RetryAll(RetryMaxAttempts(1), RetryTemporaryErr()), ConstDelay(time.Second))
	c := &http.Client{Transport: tr}
	// ResponseHeaderTimeout causes a TemporaryErr, so it will retry once
	wg.Add(2)
	res, err := c.Get(srv.URL + "/test")
	require.Nil(t, res)
	assertURLTimeoutErr(t, err)

	// test with HTTP transport, make sure it returns the same error
	c = &http.Client{Transport: httpTr}
	wg.Add(1)
	res, err = c.Get(srv.URL + "/test")
	require.Nil(t, res)
	assertURLTimeoutErr(t, err)

	wg.Wait()
}

func TestClientRetry(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	tr := NewTransport(mock, RetryAll(RetryMaxAttempts(1), RetryTemporaryErr()), ConstDelay(0))

	client := &http.Client{
		Transport: tr,
	}
	_, err := client.Get("http://example.com")
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, tempErr{}, uerr.Err)
	}
	assert.Equal(t, 2, mock.Calls())
}

func TestClientFailBufferBody(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	tr := NewTransport(mock, RetryAll(RetryMaxAttempts(1), RetryTemporaryErr()), ConstDelay(0))

	client := &http.Client{
		Transport: tr,
	}
	_, err := client.Post("http://example.com", "text/plain", iotest.TimeoutReader(strings.NewReader("hello")))
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, iotest.ErrTimeout, uerr.Err)
	}
	assert.Equal(t, 0, mock.Calls())
}

func TestClientPreventRetryWithBody(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	tr := NewTransport(mock, RetryAll(RetryMaxAttempts(1), RetryTemporaryErr()), ConstDelay(0))
	tr.PreventRetryWithBody = true

	client := &http.Client{
		Transport: tr,
	}

	_, err := client.Post("http://example.com", "text/plain", strings.NewReader("test"))
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, tempErr{}, uerr.Err)
	}
	assert.Equal(t, 1, mock.Calls()) // did not retry
	assert.Equal(t, []string{"test"}, mock.Bodies())
}

func TestClientRetryWithBody(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	tr := NewTransport(mock, RetryAll(RetryMaxAttempts(1), RetryTemporaryErr()), ConstDelay(0))

	client := &http.Client{
		Transport: tr,
	}
	_, err := client.Post("http://example.com", "text/plain", strings.NewReader("hello"))
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, tempErr{}, uerr.Err)
	}
	assert.Equal(t, 2, mock.Calls())
	assert.Equal(t, []string{"hello", "hello"}, mock.Bodies())
}

func TestClientNoRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.Path)
	}))
	defer srv.Close()

	tr := NewTransport(nil, RetryAll(RetryMaxAttempts(2), RetryTemporaryErr()), ConstDelay(time.Second))

	c := &http.Client{
		Transport: tr,
	}
	res, err := c.Get(srv.URL + "/test")
	require.Nil(t, err)
	defer res.Body.Close()

	assert.Equal(t, 200, res.StatusCode)
	var buf bytes.Buffer
	_, err = io.Copy(&buf, res.Body)
	require.Nil(t, err)
	assert.Equal(t, "/test", buf.String())
}

func TestConstDelay(t *testing.T) {
	want := 2 * time.Second
	fn := ConstDelay(want)
	for i := 0; i < 5; i++ {
		delay := fn(Attempt{Index: i})
		assert.Equal(t, want, delay, "%d", i)
	}
}

func TestExpJitterDelay(t *testing.T) {
	fn := ExpJitterDelay(time.Second, 5*time.Second)
	for i := 0; i < 10; i++ {
		delay := fn(Attempt{Index: i})
		top := math.Pow(2, float64(i)) * float64(time.Second)
		actual := time.Duration(math.Min(float64(5*time.Second), top))
		assert.True(t, delay > 0, "%d: %s <= 0", i, delay)
		assert.True(t, delay <= actual, "%d: %s > %s", i, delay, actual)
	}
}

func TestRetryHTTPMethods(t *testing.T) {
	cases := []struct {
		retries int
		meths   []string
		inMeth  string
		att     int
		want    bool
	}{
		{retries: 1, meths: nil, inMeth: "GET", att: 0, want: false},
		{retries: 0, meths: nil, inMeth: "GET", att: 1, want: false},
		{retries: 1, meths: []string{"get"}, inMeth: "GET", att: 0, want: true},
		{retries: 1, meths: []string{"GET"}, inMeth: "GET", att: 0, want: true},
		{retries: 1, meths: []string{"GET"}, inMeth: "POST", att: 0, want: false},
		{retries: 2, meths: []string{"GET", "POST"}, inMeth: "POST", att: 0, want: true},
		{retries: 2, meths: []string{"GET", "POST"}, inMeth: "POST", att: 1, want: true},
		{retries: 2, meths: []string{"GET", "POST"}, inMeth: "POST", att: 2, want: false},
		{retries: 2, meths: []string{"GET", "POST"}, inMeth: "put", att: 0, want: false},
		{retries: 2, meths: []string{"GET", "POST", "PUT"}, inMeth: "put", att: 0, want: true},
	}

	for i, tc := range cases {
		fn := RetryAll(RetryMaxAttempts(tc.retries), RetryHTTPMethods(tc.meths...))
		req, err := http.NewRequest(tc.inMeth, "", nil)
		require.Nil(t, err)
		got := fn(Attempt{Request: req, Index: tc.att})
		assert.Equal(t, tc.want, got, "%d", i)
	}
}

func TestRetryStatusRange(t *testing.T) {
	cases := []struct {
		retries int
		res     *http.Response
		att     int
		want    bool
	}{
		{retries: 1, res: nil, att: 0, want: false},
		{retries: 1, res: nil, att: 1, want: false},
		{retries: 1, res: &http.Response{StatusCode: 200}, att: 0, want: false},
		{retries: 1, res: &http.Response{StatusCode: 400}, att: 0, want: false},
		{retries: 1, res: &http.Response{StatusCode: 500}, att: 0, want: true},
		{retries: 1, res: &http.Response{StatusCode: 500}, att: 1, want: false},
		{retries: 2, res: &http.Response{StatusCode: 500}, att: 0, want: true},
		{retries: 2, res: &http.Response{StatusCode: 500}, att: 1, want: true},
		{retries: 2, res: &http.Response{StatusCode: 500}, att: 2, want: false},
	}

	for i, tc := range cases {
		fn := RetryAll(RetryMaxAttempts(tc.retries), RetryStatusRange(500, 500))
		got := fn(Attempt{Response: tc.res, Index: tc.att})
		assert.Equal(t, tc.want, got, "%d", i)
	}
}

func TestRetryStatuses(t *testing.T) {
	cases := []struct {
		retries int
		res     *http.Response
		att     int
		want    bool
	}{
		{retries: 1, res: nil, att: 0, want: false},
		{retries: 1, res: nil, att: 1, want: false},
		{retries: 1, res: &http.Response{StatusCode: 200}, att: 0, want: false},
		{retries: 1, res: &http.Response{StatusCode: 400}, att: 0, want: false},
		{retries: 1, res: &http.Response{StatusCode: 401}, att: 0, want: true},
		{retries: 1, res: &http.Response{StatusCode: 401}, att: 1, want: false},
		{retries: 1, res: &http.Response{StatusCode: 402}, att: 0, want: false},
		{retries: 1, res: &http.Response{StatusCode: 500}, att: 0, want: true},
		{retries: 1, res: &http.Response{StatusCode: 500}, att: 1, want: false},
		{retries: 2, res: &http.Response{StatusCode: 500}, att: 0, want: true},
		{retries: 2, res: &http.Response{StatusCode: 500}, att: 1, want: true},
		{retries: 2, res: &http.Response{StatusCode: 500}, att: 2, want: false},
	}

	for i, tc := range cases {
		fn := RetryAll(RetryMaxAttempts(tc.retries), RetryStatuses(401, 500))
		got := fn(Attempt{Response: tc.res, Index: tc.att})
		assert.Equal(t, tc.want, got, "%d", i)
	}
}

type tempErr struct{}

func (t tempErr) Error() string   { return "temp error" }
func (t tempErr) Temporary() bool { return true }

func TestRetryTemporaryErr(t *testing.T) {
	cases := []struct {
		retries int
		err     error
		att     int
		want    bool
	}{
		{retries: 1, err: nil, att: 0, want: false},
		{retries: 1, err: nil, att: 1, want: false},
		{retries: 1, err: io.EOF, att: 0, want: false},
		{retries: 1, err: tempErr{}, att: 0, want: true},
		{retries: 1, err: tempErr{}, att: 1, want: false},
	}

	for i, tc := range cases {
		fn := RetryAll(RetryMaxAttempts(tc.retries), RetryTemporaryErr())
		got := fn(Attempt{Index: tc.att, Error: tc.err})
		assert.Equal(t, tc.want, got, "%d", i)
	}
}

func TestRetryAll(t *testing.T) {
	max := RetryMaxAttempts(2)
	status := RetryStatusRange(500, 600)
	temp := RetryTemporaryErr()
	meths := RetryHTTPMethods("GET")
	fn := RetryAll(max, status, temp, meths)

	cases := []struct {
		method string
		status int
		att    int
		err    error
		want   bool
	}{
		{"POST", 200, 0, nil, false},
		{"GET", 200, 0, nil, false},
		{"GET", 500, 0, nil, false},
		{"GET", 500, 0, tempErr{}, true},
		{"GET", 500, 1, tempErr{}, true},
		{"GET", 500, 2, tempErr{}, false},
		{"GET", 400, 0, tempErr{}, false},
		{"GET", 500, 0, io.EOF, false},
	}
	for i, tc := range cases {
		got := fn(Attempt{
			Request:  &http.Request{Method: tc.method},
			Response: &http.Response{StatusCode: tc.status},
			Index:    tc.att,
			Error:    tc.err,
		})
		assert.Equal(t, tc.want, got, "%d", i)
	}

	// en empty RetryAll always returns true
	fn = RetryAll()
	got := fn(Attempt{Index: 0})
	assert.True(t, got, "empty RetryAll")
}

func TestRetryAny(t *testing.T) {
	max := RetryMaxAttempts(2)
	status := RetryStatusRange(500, 600)
	temp := RetryTemporaryErr()
	meths := RetryHTTPMethods("GET")
	fn := RetryAny(status, temp, meths)
	fn = RetryAll(max, fn)

	cases := []struct {
		method string
		status int
		att    int
		err    error
		want   bool
	}{
		{"POST", 200, 0, nil, false},
		{"GET", 200, 0, nil, true},
		{"POST", 500, 0, nil, true},
		{"POST", 200, 0, tempErr{}, true},
		{"POST", 200, 0, io.EOF, false},
		{"GET", 500, 0, tempErr{}, true},
		{"GET", 500, 1, tempErr{}, true},
		{"GET", 500, 2, tempErr{}, false},
	}
	for i, tc := range cases {
		got := fn(Attempt{
			Request:  &http.Request{Method: tc.method},
			Response: &http.Response{StatusCode: tc.status},
			Index:    tc.att,
			Error:    tc.err,
		})
		assert.Equal(t, tc.want, got, "%d", i)
	}

	// en empty RetryAny always returns false
	fn = RetryAny()
	got := fn(Attempt{Index: 0})
	assert.False(t, got, "empty RetryAny")
}

func TestToRetryFn(t *testing.T) {
	fn := toRetryFn(RetryAll(RetryMaxAttempts(2), RetryTemporaryErr()), ConstDelay(time.Second))

	cases := []struct {
		err       error
		att       int
		wantRetry bool
		wantDelay time.Duration
	}{
		{err: nil, att: 0, wantRetry: false, wantDelay: 0},
		{err: io.EOF, att: 0, wantRetry: false, wantDelay: 0},
		{err: tempErr{}, att: 0, wantRetry: true, wantDelay: time.Second},
		{err: tempErr{}, att: 1, wantRetry: true, wantDelay: time.Second},
		{err: tempErr{}, att: 2, wantRetry: false, wantDelay: 0},
	}

	for i, tc := range cases {
		retry, delay := fn(Attempt{Index: tc.att, Error: tc.err})
		assert.Equal(t, tc.wantRetry, retry, "%d - retry?", i)
		assert.Equal(t, tc.wantDelay, delay, "%d - delay", i)
	}
}
