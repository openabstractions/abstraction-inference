package transportws

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gorilla "github.com/openabstractions/websocket"
)

type DialOptions struct {
	HTTPClient *http.Client
	HTTPHeader http.Header
}

// Dial asks the caller's HTTP client to perform the opening request. A
// successful 101 body stays with the connection; invalid responses are closed.
func Dial(ctx context.Context, address string, opts *DialOptions) (*Conn, *http.Response, error) {
	if ctx == nil {
		return nil, nil, errors.New("nil WebSocket dial context")
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, nil, errors.New("invalid WebSocket URL")
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return nil, nil, errors.New("invalid WebSocket URL scheme")
	}
	client := http.DefaultClient
	var header http.Header
	if opts != nil {
		if opts.HTTPClient != nil {
			client = opts.HTTPClient
		}
		header = opts.HTTPHeader
	}
	// net/http's Client.Timeout otherwise remains active after the 101. Apply
	// it to the handshake context while preserving the supplied transport,
	// cookie jar and redirect policy on a private client copy.
	copyClient := *client
	if copyClient.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, copyClient.Timeout)
		defer cancel()
		copyClient.Timeout = 0
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header = header.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	// The current adapter negotiates neither subprotocols nor compression.
	req.Header.Del("Sec-WebSocket-Protocol")
	req.Header.Del("Sec-WebSocket-Extensions")
	response, err := copyClient.Do(req)
	if err != nil {
		if response != nil && response.Body != nil {
			err = errors.Join(err, response.Body.Close())
		}
		return nil, response, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols ||
		!headerToken(response.Header, "Connection", "upgrade") ||
		!headerToken(response.Header, "Upgrade", "websocket") ||
		response.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) ||
		response.Header.Get("Sec-WebSocket-Protocol") != "" ||
		response.Header.Get("Sec-WebSocket-Extensions") != "" {
		preserveFailureBody(response)
		return nil, response, errors.New("invalid WebSocket upgrade response")
	}
	body, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		preserveFailureBody(response)
		return nil, response, errors.New("WebSocket upgrade body is not writable")
	}
	return &Conn{inner: gorilla.NewClientConnFromUpgrade(&bodyConn{body: body}, "")}, response, nil
}

// Preserve a bounded refusal body for gateway callers. The timer handles a
// custom RoundTripper that stalls while returning a non-upgrade response.
func preserveFailureBody(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	body := response.Body
	// Refusal-body capture is diagnostic. The upgrade error remains authoritative
	// if the peer stalls or the body cannot be fully read and closed.
	//unchecked: timer only releases a stalled refusal body and cannot return an error
	timer := time.AfterFunc(3*time.Second, func() { _ = body.Close() })
	defer timer.Stop()
	//unchecked: partial refusal text is diagnostic and remains useful after a read error
	contents, _ := io.ReadAll(io.LimitReader(body, 1024))
	//unchecked: refusal-body close is cleanup after the upgrade error is fixed
	_ = body.Close()
	response.Body = io.NopCloser(bytes.NewReader(contents))
}

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}
func headerToken(header http.Header, field, want string) bool {
	for _, line := range header.Values(field) {
		for _, part := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(part), want) {
				return true
			}
		}
	}
	return false
}

// bodyConn keeps the private deadline behavior required by Gorilla. Its
// terminal deadline closes the 101 body, matching the adapter's cancellation
// behavior. A supplied RoundTripper must return a body whose Close releases
// blocked I/O; the standard net/http transport does.
type bodyConn struct {
	body                  io.ReadWriteCloser
	mu                    sync.Mutex
	readTimer, writeTimer *time.Timer
	once                  sync.Once
}

func (c *bodyConn) Read(p []byte) (int, error)  { return c.body.Read(p) }
func (c *bodyConn) Write(p []byte) (int, error) { return c.body.Write(p) }
func (c *bodyConn) Close() error {
	var err error
	c.once.Do(func() {
		c.mu.Lock()
		if c.readTimer != nil {
			c.readTimer.Stop()
		}
		if c.writeTimer != nil {
			c.writeTimer.Stop()
		}
		c.mu.Unlock()
		err = c.body.Close()
	})
	return err
}

type bodyAddr struct{}

func (bodyAddr) Network() string       { return "http-upgrade" }
func (bodyAddr) String() string        { return "http-upgrade" }
func (*bodyConn) LocalAddr() net.Addr  { return bodyAddr{} }
func (*bodyConn) RemoteAddr() net.Addr { return bodyAddr{} }
func (c *bodyConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
func (c *bodyConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readTimer != nil {
		c.readTimer.Stop()
	}
	c.readTimer = bodyDeadline(t, c.Close)
	return nil
}
func (c *bodyConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeTimer != nil {
		c.writeTimer.Stop()
	}
	c.writeTimer = bodyDeadline(t, c.Close)
	return nil
}
func bodyDeadline(t time.Time, closeFn func() error) *time.Timer {
	if t.IsZero() {
		return nil
	}
	d := time.Until(t)
	if d < 0 {
		d = 0
	}
	// The deadline callback has no caller to receive a close error. I/O receives
	// the terminal connection result when Close releases its blocked operation.
	//unchecked: deadline callback releases blocked I/O and has no error return path
	return time.AfterFunc(d, func() { _ = closeFn() })
}

var _ net.Conn = (*bodyConn)(nil)
