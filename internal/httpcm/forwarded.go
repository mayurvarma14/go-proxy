package httpcm

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"strconv"
	"strings"
)

// ForwardedHeaders injects common proxy headers and a request ID.
// - X-Forwarded-For: appends the downstream client IP
// - X-Forwarded-Proto: "http" or "https" based on downstream
// - Via: "<major>.<minor> go-proxy", appended to existing
// - X-Request-Id: preserves incoming or generates if missing and stores in rc.RequestID
type ForwardedHeaders struct{ proxyName string }

func NewForwardedHeaders(name string) ForwardedHeaders { return ForwardedHeaders{proxyName: name} }

func (f ForwardedHeaders) OnRequest(rc *RequestCtx) error {
	h := rc.Req.Header

	// Extract client IP
	ip := ""
	if ta, ok := rc.Remote.(*net.TCPAddr); ok {
		ip = ta.IP.String()
	}
	if ip == "" {
		// best-effort parse of host:port
		parts := strings.Split(rc.Remote.String(), ":")
		if len(parts) > 0 {
			ip = parts[0]
		}
	}
	if ip != "" {
		if xf := h.Get("X-Forwarded-For"); xf != "" {
			h.Set("X-Forwarded-For", xf+", "+ip)
		} else {
			h.Set("X-Forwarded-For", ip)
		}
	}

	// X-Forwarded-Proto
	if rc.DownstreamTLS {
		h.Set("X-Forwarded-Proto", "https")
	} else {
		h.Set("X-Forwarded-Proto", "http")
	}

	// Via header: append our marker
	viaVal := strings.TrimSpace(h.Get("Via"))
	me := strings.TrimSpace(strings.Join([]string{strconv.Itoa(rc.Req.ProtoMajor), ".", strconv.Itoa(rc.Req.ProtoMinor), " ", f.proxyName}, ""))
	if viaVal == "" {
		h.Set("Via", me)
	} else {
		h.Set("Via", viaVal+", "+me)
	}

	// X-Request-Id: preserve or generate
	rid := h.Get("X-Request-Id")
	if rid == "" {
		rid = genID()
		h.Set("X-Request-Id", rid)
	}
	rc.RequestID = rid
	return nil
}

func genID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}
