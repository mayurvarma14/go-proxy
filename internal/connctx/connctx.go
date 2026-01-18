package connctx

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

// ConnCtx bundles per-connection state.
type ConnCtx struct {
	Context context.Context
	Conn    net.Conn
	Remote  net.Addr
	Start   time.Time
	TLS     *tls.ConnectionState
}
