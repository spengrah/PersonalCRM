package decision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"

	"personal-crm/backend/internal/threadctx"
)

// Adapter sends a masked request to a vendor's route. It accepts nothing but
// the masker's MaskedRequest, so no unmasked content reaches a route through
// it, and keeps no request content.
type Adapter interface {
	Route() Route
	Send(ctx context.Context, req threadctx.MaskedRequest) (Answers, error)
}

// NewMaskedMethod builds a method that masks every request before its adapter
// sends it.
func NewMaskedMethod(def Definition, masker *threadctx.Masker, adapter Adapter) Method {
	return newMethod(def, adapter.Route(), true, func(ctx context.Context, c threadctx.Context, questions []threadctx.Question) (prepared, error) {
		req, err := masker.Mask(ctx, c, questions)
		if err != nil {
			return prepared{}, fmt.Errorf("mask request: %w", err)
		}
		return prepared{wire: renderMasked(req), send: func(ctx context.Context) (Answers, error) { return adapter.Send(ctx, req) }}, nil
	})
}

// LocalEndpoint is a System One endpoint on the user's own machines: a
// loopback, private-network or tailnet address. Only NewLocalEndpoint builds
// one.
type LocalEndpoint struct{ url string }

// tailnet is the address range Tailscale assigns.
var tailnet = netip.MustParsePrefix("100.64.0.0/10")

// NewLocalEndpoint accepts a base URL whose host is localhost or an IP
// address that is loopback, private or on the tailnet, and refuses any other,
// since a host name may resolve anywhere.
func NewLocalEndpoint(baseURL string) (LocalEndpoint, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return LocalEndpoint{}, fmt.Errorf("local endpoint %q is not a URL", baseURL)
	}
	host := u.Hostname()
	if host == "localhost" {
		return LocalEndpoint{url: baseURL}, nil
	}
	if addr, err := netip.ParseAddr(host); err == nil && (addr.IsLoopback() || addr.IsPrivate() || tailnet.Contains(addr.Unmap())) {
		return LocalEndpoint{url: baseURL}, nil
	}
	return LocalEndpoint{}, fmt.Errorf("endpoint host %q is not a loopback, private or tailnet address", host)
}

// String returns the endpoint's base URL.
func (e LocalEndpoint) String() string { return e.url }

// errRedirected is a call to a local endpoint that answered with a redirect.
var errRedirected = errors.New("local endpoint redirected the call")

// client calls the endpoint and follows no redirect, since a redirect may
// lead off the user's machines and the request body would be sent again there.
func (e LocalEndpoint) client() *http.Client {
	return &http.Client{Timeout: callTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirected }}
}

// NewUnmaskedMethod builds a method that sends requests unmasked to a local
// System One server, the only route unmasked content may take.
func NewUnmaskedMethod(def Definition, endpoint LocalEndpoint, apiKey, model string) Method {
	client := &systemOne{url: endpoint.url, apiKey: apiKey, model: model, http: endpoint.client()}
	return newMethod(def, client.Route(), false, func(ctx context.Context, c threadctx.Context, questions []threadctx.Question) (prepared, error) {
		wire := render(c.Kind, c.CutOff, c.Entries, questions)
		return prepared{wire: wire, send: func(ctx context.Context) (Answers, error) { return client.post(ctx, wire) }}, nil
	})
}
