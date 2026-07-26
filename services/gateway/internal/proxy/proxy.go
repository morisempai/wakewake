// Package proxy builds the reverse proxy to a single internal service.
//
// The gateway does not interpret the request beyond its path prefix: the full path, query, method,
// headers, and body pass through unchanged so each service sees exactly the request the client
// sent. The correlation id rides along via the transport, and upstream failures are rendered as the
// shared error envelope rather than the bare 502 httputil emits by default.
package proxy

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/httpx"
)

// codeBadGateway is the error code the gateway returns when an upstream cannot be reached.
const codeBadGateway = "bad_gateway"

// New builds a reverse proxy to the given upstream base URL (scheme + host, no path). The incoming
// request path and query are preserved, so a request for /v1/products/{id} arrives at the service
// as /v1/products/{id}.
func New(target string, log *slog.Logger) (*httputil.ReverseProxy, error) {
	base, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid upstream url %q: %w", target, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("proxy: upstream url %q must be absolute (scheme and host)", target)
	}

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL points the outbound request at the upstream and joins the (empty) base path
			// with the incoming path, leaving the path the client sent intact.
			pr.SetURL(base)
			pr.Out.Host = base.Host
			// Preserve the original client details for any downstream that logs them; the gateway's
			// own rate limiting uses the real connection address, not these headers.
			pr.SetXForwarded()
		},
		// The correlation RoundTripper stamps the id onto the outbound request when the client did
		// not send one, so a request minted at the edge stays traceable through the upstream.
		Transport: correlation.RoundTripper{Base: newTransport()},
		ModifyResponse: func(res *http.Response) error {
			// The edge already echoed the correlation id on the response (correlation.Middleware);
			// drop any copy the upstream also set so the client sees exactly one value.
			res.Header.Del(correlation.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Do not leak the upstream address or the transport error to the client; log it for
			// operators and return a generic 502.
			log.ErrorContext(r.Context(), "upstream request failed",
				slog.String("upstream", base.Redacted()),
				slog.String("path", r.URL.Path),
				slog.String("error", err.Error()))
			httpx.WriteError(w, r, http.StatusBadGateway, codeBadGateway,
				"The upstream service is unavailable.")
		},
		// Route httputil's internal logging through the structured logger rather than the default
		// stderr, so gateway logs stay one JSON stream.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
	}, nil
}

// newTransport is a bounded HTTP transport: every dial and handshake has a deadline so a slow or
// black-holed upstream cannot pin a gateway connection indefinitely.
func newTransport() http.RoundTripper {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
