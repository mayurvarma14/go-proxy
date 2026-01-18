package accesslog

import (
    "fmt"
    "time"
)

type Entry struct {
    Time     time.Time
    Remote   string
    Method   string
    Path     string
    Status   int
    Duration time.Duration
    Cluster  string
    Upstream string
    RequestID string
}

// Log prints a single-line access log to stdout.
func Log(e Entry) {
    // Simple Common Log-ish format
    fmt.Printf("%s remote=%s method=%s path=%s status=%d dur_ms=%d cluster=%s upstream=%s req_id=%s\n",
        e.Time.Format(time.RFC3339Nano), e.Remote, e.Method, e.Path, e.Status, e.Duration.Milliseconds(), e.Cluster, e.Upstream, e.RequestID,
    )
}
