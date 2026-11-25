package updater

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// TLS to GitHub is established by the root updater over this opaque connection.
// Neither the unprivileged node nor the hub can substitute a manifest or binary.
func RelayDial(listen string) (func(context.Context, string, string) (net.Conn, error), error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("更新中转只允许回环维护地址")
	}
	endpoint := "http://" + net.JoinHostPort(host, port) + "/api/update/relay?address="
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if !strings.HasPrefix(network, "tcp") || !AllowedAddress(address) {
			return nil, errors.New("更新中转目标不允许")
		}
		c := &http.Client{Transport: &http.Transport{}}
		ws, _, err := websocket.Dial(ctx, endpoint+url.QueryEscape(address), &websocket.DialOptions{HTTPClient: c})
		if err != nil {
			c.CloseIdleConnections()
			return nil, err
		}
		// DialContext governs establishment, not the lifetime of a pooled Conn.
		return websocket.NetConn(context.Background(), ws, websocket.MessageBinary), nil
	}, nil
}
