package transportws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gorilla "github.com/openabstractions/websocket"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDialPreservesCustomTLSClientAndHandshakeTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&gorilla.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(gorilla.TextMessage, []byte("ready"))
	}))
	defer server.Close()
	client := server.Client()
	base := client.Transport
	var calls atomic.Int64
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return base.RoundTrip(req)
	})
	client.Timeout = time.Second
	conn, _, err := Dial(context.Background(), "wss"+strings.TrimPrefix(server.URL, "https"), &DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	kind, body, err := conn.Read(ctx)
	if err != nil || kind != MessageText || string(body) != "ready" || calls.Load() != 1 {
		t.Fatalf("TLS custom client: kind=%d body=%q calls=%d err=%v", kind, body, calls.Load(), err)
	}

	blocked := &http.Client{Timeout: 20 * time.Millisecond, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	started := time.Now()
	if c, _, err := Dial(context.Background(), "ws://timeout.invalid/", &DialOptions{HTTPClient: blocked}); err == nil {
		c.CloseNow()
		t.Fatal("client timeout was ignored")
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("client timeout took %v", took)
	}
}

func TestTLSUpgradeSurvivesClientHandshakeTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := (&gorilla.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer peer.Close()
		kind, body, err := peer.ReadMessage()
		if err == nil {
			_ = peer.WriteMessage(kind, body)
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second
	conn, _, err := Dial(context.Background(), "wss"+strings.TrimPrefix(server.URL, "https"), &DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	time.Sleep(1200 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, MessageText, []byte("after timeout")); err != nil {
		t.Fatalf("write after handshake timeout: %v", err)
	}
	kind, body, err := conn.Read(ctx)
	if err != nil || kind != MessageText || string(body) != "after timeout" {
		t.Fatalf("read after handshake timeout: kind=%d body=%q err=%v", kind, body, err)
	}
}

func TestDialPreservesRedirectPolicyAndRejectsUnnegotiatedExtension(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	conn, response, err := Dial(context.Background(), "ws"+strings.TrimPrefix(source.URL, "http"),
		&DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer private"}}})
	if conn != nil || err == nil || response == nil || response.StatusCode != http.StatusTemporaryRedirect || redirected.Load() != 0 {
		t.Fatalf("redirect policy: conn=%v response=%v target calls=%d err=%v", conn, response, redirected.Load(), err)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}

	client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusSwitchingProtocols, Request: req,
			Header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
				"Sec-Websocket-Accept":     {acceptKey(req.Header.Get("Sec-WebSocket-Key"))},
				"Sec-Websocket-Extensions": {"permessage-deflate"}},
			Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	conn, _, err = Dial(context.Background(), "ws://synthetic.invalid/", &DialOptions{HTTPClient: client})
	if conn != nil || err == nil {
		t.Fatalf("unnegotiated extension accepted: conn=%v err=%v", conn, err)
	}
}

type opaqueBody struct{ io.ReadWriteCloser }
type signaledBody struct {
	io.ReadWriteCloser
	started chan<- string
}

func (b signaledBody) Read(p []byte) (int, error) {
	select {
	case b.started <- "read":
	default:
	}
	return b.ReadWriteCloser.Read(p)
}
func (b signaledBody) Write(p []byte) (int, error) {
	select {
	case b.started <- "write":
	default:
	}
	return b.ReadWriteCloser.Write(p)
}

func syntheticClient(peer func(net.Conn), started ...chan<- string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		client, server := net.Pipe()
		go peer(server)
		var body io.ReadWriteCloser = opaqueBody{client}
		if len(started) != 0 {
			body = signaledBody{ReadWriteCloser: body, started: started[0]}
		}
		return &http.Response{StatusCode: 101, Status: "101 Switching Protocols", Request: req,
			Header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
				"Sec-Websocket-Accept": {acceptKey(req.Header.Get("Sec-WebSocket-Key"))}},
			Body: body}, nil
	})}
}

func rawFrame(first byte, payload []byte) []byte {
	if len(payload) > 65535 {
		panic("fixture frame too large")
	}
	frame := []byte{first}
	if len(payload) <= 125 {
		frame = append(frame, byte(len(payload)))
	} else {
		frame = append(frame, 126, byte(len(payload)>>8), byte(len(payload)))
	}
	return append(frame, payload...)
}

func readMaskedFrame(r io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	if header[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame is unmasked")
	}
	length := int(header[1] & 0x7f)
	if length == 126 {
		var more [2]byte
		if _, err := io.ReadFull(r, more[:]); err != nil {
			return 0, nil, err
		}
		length = int(more[0])<<8 | int(more[1])
	} else if length == 127 {
		return 0, nil, errors.New("fixture long frame")
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return header[0], payload, nil
}

func TestIndependentPeerMaskPingAndFragment(t *testing.T) {
	peerDone := make(chan error, 1)
	client := syntheticClient(func(peer net.Conn) {
		defer peer.Close()
		first, payload, err := readMaskedFrame(peer)
		if err != nil || first != 0x81 || string(payload) != "client" {
			peerDone <- fmt.Errorf("client frame %x %q: %w", first, payload, err)
			return
		}
		if _, err := peer.Write(rawFrame(0x89, []byte("hi"))); err != nil {
			peerDone <- err
			return
		}
		first, payload, err = readMaskedFrame(peer)
		if err != nil || first != 0x8a || string(payload) != "hi" {
			peerDone <- fmt.Errorf("pong %x %q: %w", first, payload, err)
			return
		}
		if _, err := peer.Write(rawFrame(0x01, []byte("frag"))); err != nil {
			peerDone <- err
			return
		}
		if _, err := peer.Write(rawFrame(0x80, []byte("ment"))); err != nil {
			peerDone <- err
			return
		}
		peerDone <- nil
	})
	conn, _, err := Dial(context.Background(), "ws://synthetic.invalid/", &DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, MessageText, []byte("client")); err != nil {
		t.Fatal(err)
	}
	kind, body, err := conn.Read(ctx)
	if err != nil || kind != MessageText || string(body) != "fragment" {
		t.Fatalf("fragment %d %q %v", kind, body, err)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}

func TestIndependentPeerTextUTF8AcrossFragments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		frames  [][]byte
		want    string
		invalid bool
	}{
		{"invalid", [][]byte{rawFrame(0x81, []byte{0xff})}, "", true},
		{"valid-split-rune", [][]byte{rawFrame(0x01, []byte{'a', 0xe2}), rawFrame(0x80, []byte{0x82, 0xac, 'b'})}, "a€b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := syntheticClient(func(peer net.Conn) {
				defer peer.Close()
				for _, frame := range tc.frames {
					if _, err := peer.Write(frame); err != nil {
						return
					}
				}
			})
			conn, _, err := Dial(context.Background(), "ws://synthetic.invalid/", &DialOptions{HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			kind, body, err := conn.Read(ctx)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "invalid UTF-8") || body != nil {
					t.Fatalf("invalid UTF-8 result: kind=%d body=%x err=%v", kind, body, err)
				}
				return
			}
			if err != nil || kind != MessageText || string(body) != tc.want {
				t.Fatalf("fragmented UTF-8: kind=%d body=%x err=%v", kind, body, err)
			}
		})
	}
}

func TestIndependentFragmentAggregateCaps(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limit, size int
	}{
		{"at-128k", 128 << 10, 128 << 10}, {"over-128k", 128 << 10, (128 << 10) + 1},
		{"at-1m", 1 << 20, 1 << 20}, {"over-1m", 1 << 20, (1 << 20) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := syntheticClient(func(peer net.Conn) {
				defer peer.Close()
				left, first := tc.size, true
				for left > 0 {
					n := min(left, 32768)
					left -= n
					flag := byte(0x00)
					if first {
						flag = 0x01
						first = false
					}
					if left == 0 {
						flag |= 0x80
					}
					if _, err := peer.Write(rawFrame(flag, []byte(strings.Repeat("x", n)))); err != nil {
						return
					}
				}
			})
			conn, _, err := Dial(context.Background(), "ws://synthetic.invalid/", &DialOptions{HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			conn.SetReadLimit(int64(tc.limit))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, body, err := conn.Read(ctx)
			if tc.size <= tc.limit {
				if err != nil || len(body) != tc.size {
					t.Fatalf("within cap: %d %v", len(body), err)
				}
			} else if !errors.Is(err, ErrMessageTooBig) {
				t.Fatalf("over cap: %d %v", len(body), err)
			}
		})
	}
}

func TestCancellationUnblocksReadAndWrite(t *testing.T) {
	for _, op := range []string{"read", "write"} {
		t.Run(op, func(t *testing.T) {
			peerOpen := make(chan net.Conn, 1)
			started := make(chan string, 1)
			client := syntheticClient(func(peer net.Conn) { peerOpen <- peer }, started)
			conn, _, err := Dial(context.Background(), "ws://synthetic.invalid/", &DialOptions{HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			peer := <-peerOpen
			defer peer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				if op == "read" {
					_, _, err := conn.Read(ctx)
					done <- err
				} else {
					done <- conn.Write(ctx, MessageText, []byte("blocked"))
				}
			}()
			select {
			case entered := <-started:
				if entered != op {
					t.Fatalf("entered %s, want %s", entered, op)
				}
			case <-time.After(time.Second):
				t.Fatal("I/O did not reach the body")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancel did not release I/O")
			}
		})
	}
}
