package httpcm

import (
    "net/http"
    "net/url"
    "strings"
)

// newHTTPRequest constructs a minimal *http.Request for tests without external deps.
func newHTTPRequest(method, path string) (*http.Request, error) {
    u := &url.URL{Scheme: "http", Host: "example", Path: path}
    return http.NewRequest(method, u.String(), strings.NewReader(""))
}

