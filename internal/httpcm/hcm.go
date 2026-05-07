package httpcm

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mayurvarma14/go-proxy/internal/connctx"
	"github.com/mayurvarma14/go-proxy/internal/obs/accesslog"
	"github.com/mayurvarma14/go-proxy/internal/obs/metrics"
)

// HCM is a network filter that parses HTTP/1.1 requests and runs HTTP filters.
type HCM struct {
	httpFilters Chain
}

// New returns a minimal HCM with a logging HTTP filter.
func New() HCM { return HCM{httpFilters: NewChain([]HTTPFilter{LoggingFilter{}})} }

// NewWithFilters builds an HCM with a provided HTTP filter chain.
func NewWithFilters(filters []HTTPFilter) HCM { return HCM{httpFilters: NewChain(filters)} }

// Handle implements the NetworkFilter interface. It serves multiple requests on
// one connection (HTTP/1.1 keep-alive) and returns on EOF or shutdown.
func (h HCM) Handle(c *connctx.ConnCtx) error {
	br := bufio.NewReader(c.Conn)
	bw := bufio.NewWriter(c.Conn)
	defer bw.Flush()

	for {
		// Attempt to read the next request; shutdown closes the conn so reads fail.
		req, err := http.ReadRequest(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if ne, ok := err.(net.Error); ok && (ne.Timeout() || !ne.Temporary()) {
				// Likely due to shutdown closing the conn; respect context.
				select {
				case <-c.Context.Done():
					return c.Context.Err()
				default:
				}
			}
			// For parse errors or closed connection, end the loop.
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		rc := &RequestCtx{
			Context:       c.Context,
			Req:           req,
			Remote:        c.Remote,
			Start:         time.Now(),
			W:             bw,
			DownstreamTLS: c.TLS != nil,
		}
		metrics.Inc("downstream_http_requests_total")
		err = h.httpFilters.Run(rc)

		// If a filter produced and wrote a response, move to next (or close).
		if rc.Responded {
			if err := bw.Flush(); err != nil {
				return err
			}
			status := rc.StatusCode
			if status == 0 {
				status = http.StatusOK
			}
			accesslog.Log(accesslog.Entry{
				Time:      rc.Start,
				Remote:    rc.Remote.String(),
				Method:    rc.Req.Method,
				Path:      rc.Req.URL.Path,
				Status:    status,
				Duration:  time.Since(rc.Start),
				Cluster:   rc.UpstreamCluster,
				Upstream:  rc.UpstreamAddr,
				RequestID: rc.RequestID,
			})
			metrics.IncDownstreamRequest(rc.Req.Method, rc.UpstreamCluster)
			metrics.IncCode(status)
			if req.Close {
				return nil
			}
			continue
		}

		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			body := "bad gateway\n"
			if status == http.StatusGatewayTimeout {
				body = "gateway timeout\n"
			}
			resp := http.Response{
				StatusCode:    status,
				ProtoMajor:    req.ProtoMajor,
				ProtoMinor:    req.ProtoMinor,
				Header:        make(http.Header),
				ContentLength: int64(len(body)),
				Body:          io.NopCloser(strings.NewReader(body)),
				Close:         req.Close,
				Request:       req,
			}
			resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
			if err := resp.Write(bw); err != nil {
				return err
			}
			if err := bw.Flush(); err != nil {
				return err
			}
			accesslog.Log(accesslog.Entry{
				Time:      rc.Start,
				Remote:    rc.Remote.String(),
				Method:    rc.Req.Method,
				Path:      rc.Req.URL.Path,
				Status:    status,
				Duration:  time.Since(rc.Start),
				Cluster:   rc.UpstreamCluster,
				Upstream:  rc.UpstreamAddr,
				RequestID: rc.RequestID,
			})
			metrics.IncDownstreamRequest(rc.Req.Method, rc.UpstreamCluster)
			metrics.IncCode(status)
			if resp.Close {
				return nil
			}
			continue
		}

		// Default responses if nothing handled it (useful for testing router-only).
		var (
			status int
			body   string
		)
		if rc.UpstreamCluster == "" {
			status = http.StatusNotFound
			body = "no route\n"
		} else {
			status = http.StatusOK
			body = "routed to: " + rc.UpstreamCluster + "\n"
		}

		resp := http.Response{
			StatusCode:    status,
			ProtoMajor:    req.ProtoMajor,
			ProtoMinor:    req.ProtoMinor,
			Header:        make(http.Header),
			ContentLength: int64(len(body)),
			Body:          io.NopCloser(strings.NewReader(body)),
			Close:         req.Close,
			Request:       req,
		}
		resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
		if err := resp.Write(bw); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		accesslog.Log(accesslog.Entry{
			Time:      rc.Start,
			Remote:    rc.Remote.String(),
			Method:    rc.Req.Method,
			Path:      rc.Req.URL.Path,
			Status:    status,
			Duration:  time.Since(rc.Start),
			Cluster:   rc.UpstreamCluster,
			Upstream:  rc.UpstreamAddr,
			RequestID: rc.RequestID,
		})
		metrics.IncDownstreamRequest(rc.Req.Method, rc.UpstreamCluster)
		metrics.IncCode(status)
		if resp.Close {
			return nil
		}
	}
}
