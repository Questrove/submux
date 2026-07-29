package runtimeipc

import (
	"context"
	"net"

	"submux/internal/runtimeapi"
)

type LocalListener interface {
	net.Listener
	PeerIdentity(net.Conn) (runtimeapi.PeerIdentity, error)
}

func Listen(endpoint string) (LocalListener, error) {
	return listenLocal(endpoint)
}

func dialContext(ctx context.Context, endpoint string) (net.Conn, error) {
	return dialLocal(ctx, endpoint)
}
