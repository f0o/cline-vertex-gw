package provider

import (
	"net/http"
)

type routingTierRoundTripper struct {
	underlying http.RoundTripper
}

func (rt *routingTierRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if tier, ok := req.Context().Value(ContextKeyRoutingTier).(string); ok && tier != "" {
		cloned.Header.Set("X-Vertex-AI-Routing-Tier", tier)
	}
	underlying := rt.underlying
	if underlying == nil {
		underlying = http.DefaultTransport
	}
	return underlying.RoundTrip(cloned)
}
