package httpcm

import (
	"fmt"
)

// LoggingFilter logs the basic request line and duration.
type LoggingFilter struct{}

func (LoggingFilter) OnRequest(rc *RequestCtx) error {
	fmt.Printf("http request method=%s path=%s remote=%s\n", rc.Req.Method, rc.Req.URL.Path, rc.Remote)
	return nil
}
