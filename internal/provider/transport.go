// This file is the one place both provider clients (runpod.New, verda.New) get their
// *http.Client from, so a dead pooled connection is detected and a slow or failed call is logged.
package provider

import (
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"
)

// pingTimeout is both HTTP2Config.SendPingTimeout and HTTP2Config.PingTimeout.
//
// [DOC] `go doc net/http.HTTP2Config`: "SendPingTimeout is the timeout after which a health check
// using a ping frame will be carried out if no frame is received on a connection. If zero, no
// health check is performed." Go's DefaultTransport leaves it at zero, so a pooled HTTP/2
// connection to RunPod that goes silently dead is never probed and every call queued onto it times
// out until the pool is discarded (docs/providers.md#runpod). PingTimeout's own zero default is
// already 15 seconds; it is set explicitly so both halves of the health check are visible here.
const pingTimeout = 15 * time.Second

// idleConnTimeout drops a pooled connection that sat idle this long rather than trusting it.
// Go's DefaultTransport uses 90 seconds. This program polls each provider every 20 seconds, so a
// connection idle for a full minute has already missed two poll cycles, and the limit is set
// tighter on purpose.
const idleConnTimeout = 60 * time.Second

// sharedTransport is the one *http.Transport both provider clients build their *http.Client on:
// Go's default transport cloned, so its dial timeout, keep-alive, MaxIdleConns and
// TLSHandshakeTimeout stay as Go sets them, with only the ping health check and the idle timeout
// added on top. ForceAttemptHTTP2 stays as DefaultTransport sets it (true).
var sharedTransport = buildSharedTransport()

func buildSharedTransport() *http.Transport {
	// http.DefaultTransport is declared as the http.RoundTripper interface and Clone is only on
	// *http.Transport, so the assertion is needed. It cannot fail: the variable holds the
	// library's own *http.Transport unless a program replaces it, and this one does not.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: pingTimeout,
		PingTimeout:     pingTimeout,
	}
	t.IdleConnTimeout = idleConnTimeout
	return t
}

// slowCallThreshold is how long an outbound provider call may take before it earns a log line on
// its own, even without an error.
const slowCallThreshold = 5 * time.Second

// UserAgent is the one User-Agent every request this program sends carries: a default desktop
// Chrome string that names no person, project or tool. A custom or library-default agent is a
// signature in the far end's access log, and RunPod's proxy (Cloudflare in front of it) refuses
// Python's default one with 403. [DOC] whatismybrowser.com/guides/the-latest-user-agent/chrome,
// read 2026-10-12 (Chrome 151). The version ages: read the page again rather than inventing one.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"

// userAgentTransport sends every request with UserAgent and nothing else, replacing any agent the
// request already carried.
type userAgentTransport struct{ base http.RoundTripper }

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", UserAgent)
	return t.base.RoundTrip(req)
}

// NewUserAgentTransport wraps base so every request it carries has UserAgent, for the clients
// that talk to a pod (readiness probes, the fixed address).
func NewUserAgentTransport(base http.RoundTripper) http.RoundTripper {
	return userAgentTransport{base: base}
}

// LogFunc is how a provider client reports a slow or failing outbound call. It is never called
// with a credential value or a request or response body, only the method, the path, the duration
// and whether the connection was reused.
type LogFunc func(text string)

// loggingRoundTripper wraps sharedTransport and writes one line through logf for a call that
// errors, or takes longer than slowCallThreshold, naming the provider, the method and path, the
// duration, and whether the connection was reused (`httptrace.ClientTrace.GotConn`'s `Reused`,
// `go doc net/http/httptrace.GotConnInfo`: "Reused is whether this connection has been previously
// used for another HTTP request."). A fast successful call writes nothing.
type loggingRoundTripper struct {
	providerName string
	logf         LogFunc
}

func (rt *loggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", UserAgent)
	var reused bool
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	start := time.Now()
	resp, err := sharedTransport.RoundTrip(req)
	duration := time.Since(start)

	if err == nil && duration <= slowCallThreshold {
		return resp, err
	}
	if rt.logf == nil {
		return resp, err
	}
	// resp is nil whenever err is non-nil, so resp.StatusCode is read only on the success branch.
	var outcome string
	if err != nil {
		outcome = "error: " + err.Error()
	} else {
		outcome = fmt.Sprintf("status %d", resp.StatusCode)
	}
	rt.logf(fmt.Sprintf(
		"%s %s %s: %s, %.1fs, connection reused: %t",
		rt.providerName, req.Method, req.URL.Path, outcome, duration.Seconds(), reused,
	))
	return resp, err
}

// NewHTTPClient builds the *http.Client every provider constructor (runpod.New, verda.New)
// uses: sharedTransport, wrapped so a call that errors or runs past slowCallThreshold writes one
// line through logf. providerName is what the log line names the call by ("RunPod", "Verda");
// timeout is that provider's own callTimeout constant.
func NewHTTPClient(providerName string, timeout time.Duration, logf LogFunc) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &loggingRoundTripper{providerName: providerName, logf: logf},
	}
}

// Truncate returns b as text, cut to at most n bytes. A provider's answer body is cut this way
// before it goes into an error message or a log line.
func Truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// RedactSecrets replaces every occurrence of serve's APIKey and HFToken in s with "<redacted>".
// It is used on a provider's answer to a create before the text reaches the log: the create's
// body carries both, and the log is read back over the network by GET /api/log.
func RedactSecrets(s string, serve *ServeOptions) string {
	if serve == nil {
		return s
	}
	if serve.APIKey != "" {
		s = strings.ReplaceAll(s, serve.APIKey, "<redacted>")
	}
	if serve.HFToken != "" {
		s = strings.ReplaceAll(s, serve.HFToken, "<redacted>")
	}
	return s
}
