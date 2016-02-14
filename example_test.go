package rehttp_test

import (
	"net/http"
	"time"

	"github.com/icerink/rehttp"
)

func Example() {
	tr, err := rehttp.NewTransport(
		nil, // will use http.DefaultTransport
		rehttp.RetryTemporaryErr(3),    // max 3 retries for Temporary errors
		rehttp.ConstDelay(time.Second), // wait 1s between retries
	)
	if err != nil {
		// handle err
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   10 * time.Second, // Client timeout applies to all retries as a whole
	}
	res, err := client.Get("http://example.com")
	if err != nil {
		// handle err
	}
	// handle response...
	res.Body.Close()
}
