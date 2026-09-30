package transport

import (
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	SSH    byte = 1
	Status byte = 2
	TCP    byte = 3
)

func Config() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = 10 * time.Second
	c.ConnectionWriteTimeout = 15 * time.Second
	c.StreamOpenTimeout = 10 * time.Second
	c.StreamCloseTimeout = 10 * time.Second
	c.LogOutput = io.Discard
	return c
}

// Preserve TCP half-close so interactive SSH and large transfers finish cleanly.
func Bridge(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{})
	copyOne := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go func() { copyOne(a, b); close(done) }()
	copyOne(b, a)
	<-done
}
