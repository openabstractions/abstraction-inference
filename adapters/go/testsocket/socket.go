// Package testsocket exposes the provider adapter's WebSocket test peer to
// runtime integration fixtures without adding a separate transport import.
package testsocket

import websocket "github.com/openabstractions/abstraction-inference/adapters/go/transportws"

type Conn = websocket.Conn
type DialOptions = websocket.DialOptions
type AcceptOptions = websocket.AcceptOptions
type MessageType = websocket.MessageType

const MessageText = websocket.MessageText

var Dial = websocket.Dial
var Accept = websocket.Accept
