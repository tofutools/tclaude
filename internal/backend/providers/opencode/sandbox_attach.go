package opencode

import (
	"context"
	"net"
	"sync"
)

// The native attach client accepts an HTTP endpoint, not a Unix socket. This
// presentation-only listener forwards to the already-owned sandbox server; it
// never starts another workload or changes the server's selected boundary.
func (r *Runtime) sandboxAttachmentEndpoint(ctx context.Context) (string, func(), error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	relayCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(relayCtx, func() { _ = listener.Close() })
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer connection.Close()
				upstream, err := r.dialSandboxControl(relayCtx)
				if err != nil {
					return
				}
				defer upstream.Close()
				relayConnectedStream(relayCtx, connection, upstream)
			}()
		}
	}()
	var once sync.Once
	closeRelay := func() {
		once.Do(func() {
			cancel()
			_ = listener.Close()
			workers.Wait()
			stop()
		})
	}
	return "http://" + listener.Addr().String(), closeRelay, nil
}
