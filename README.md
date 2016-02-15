# rehttp [![build status](https://secure.travis-ci.org/PuerkitoBio/rehttp.png)](http://travis-ci.org/PuerkitoBio/rehttp) [![GoDoc](https://godoc.org/github.com/PuerkitoBio/rehttp?status.png)][godoc]

Package rehttp implements an HTTP Transport (an `http.RoundTripper`) that handles retries. See the [godoc][] for details.

## Installation

Please note that rehttp requires Go 1.5+, because it uses the `http.Request.Cancel` field to check for cancelled requests.

    $ go get github.com/PuerkitoBio/rehttp

## License

The [BSD 3-Clause license][bsd].

[bsd]: http://opensource.org/licenses/BSD-3-Clause
[godoc]: http://godoc.org/github.com/PuerkitoBio/rehttp
