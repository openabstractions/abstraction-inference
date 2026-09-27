// Package transportws adapts the reviewed OpenAbstractions Gorilla fork to
// the inference adapter's context-based WebSocket operations.
package transportws

import (
	"context"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	gorilla "github.com/openabstractions/websocket"
)

type MessageType int

const (
	MessageText         MessageType = gorilla.TextMessage
	StatusNormalClosure             = gorilla.CloseNormalClosure
)

var ErrMessageTooBig = gorilla.ErrReadLimit

// Conn owns one upgraded connection. Reads and writes may proceed in parallel;
// a canceled operation closes the connection so blocked I/O cannot outlive it.
type Conn struct{ inner *gorilla.Conn }

func (c *Conn) SetReadLimit(limit int64) { c.inner.SetReadLimit(limit) }

func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	if ctx == nil {
		return 0, nil, errors.New("nil WebSocket read context")
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	//unchecked: cancellation returns the context error while this callback releases blocked I/O
	stop := context.AfterFunc(ctx, func() { _ = c.CloseNow() })
	defer stop()
	kind, payload, err := c.inner.ReadMessage()
	if ctx.Err() != nil {
		return 0, nil, ctx.Err()
	}
	if err == nil && kind == gorilla.TextMessage && !utf8.Valid(payload) {
		return 0, nil, errors.Join(errors.New("invalid UTF-8 in WebSocket text message"), c.CloseNow())
	}
	return MessageType(kind), payload, err
}

func (c *Conn) Write(ctx context.Context, kind MessageType, payload []byte) error {
	if ctx == nil {
		return errors.New("nil WebSocket write context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	//unchecked: cancellation returns the context error while this callback releases blocked I/O
	stop := context.AfterFunc(ctx, func() { _ = c.CloseNow() })
	defer stop()
	err := c.inner.WriteMessage(int(kind), payload)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (c *Conn) CloseNow() error { return c.inner.Close() }

func (c *Conn) Close(status int, reason string) error {
	writeErr := c.inner.WriteControl(gorilla.CloseMessage, gorilla.FormatCloseMessage(status, reason), time.Now().Add(time.Second))
	return errors.Join(writeErr, c.inner.Close())
}

type CompressionMode int

const CompressionDisabled CompressionMode = 0

type AcceptOptions struct{ CompressionMode CompressionMode }

func Accept(w http.ResponseWriter, r *http.Request, opts *AcceptOptions) (*Conn, error) {
	if opts != nil && opts.CompressionMode != CompressionDisabled {
		return nil, errors.New("unsupported WebSocket compression mode")
	}
	upgrader := gorilla.Upgrader{EnableCompression: false}
	inner, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	return &Conn{inner: inner}, nil
}
