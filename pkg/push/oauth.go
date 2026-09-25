package push

import (
	"net/http"

	"golang.org/x/oauth2"
)

// oauth2Transport adds a Bearer token from an oauth2.TokenSource to requests.
type oauth2Transport struct {
	src  oauth2.TokenSource
	base http.RoundTripper
}

func (t *oauth2Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.src.Token()
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
