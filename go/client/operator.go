package client

import (
	"context"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// Operator calls abstraction.inference/operator@1: the runtime's inference
// hosts, the gateway window's local keys and the inference audit. Each call is
// a rights decision the runtime makes for this program.
type Operator struct{ transport listen.FrameClient }

// NewOperator binds an explicit endpoint.
func NewOperator(endpoint string) *Operator {
	return NewOperatorWithTransport(listen.FrameClient{Endpoint: endpoint})
}

// NewOperatorWithTransport retains the caller's endpoint, server trust and limits.
func NewOperatorWithTransport(transport listen.FrameClient) *Operator {
	return &Operator{transport: transport.WithDefaults(callMargin, 1<<20)}
}

func (o *Operator) client(ctx context.Context) (*wire.OperatorClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return wire.NewOperatorClient(o.transport.WithContext(ctx)), nil
}

// Hosts reads the configured hosts, their state and each credential's spend.
func (o *Operator) Hosts(ctx context.Context) (HostList, error) {
	c, err := o.client(ctx)
	if err != nil {
		return HostList{}, err
	}
	return c.Hosts()
}

// AddHost adds one host at the listed configuration revision.
func (o *Operator) AddHost(ctx context.Context, expectedRevision string, host HostEntry) (HostChange, error) {
	c, err := o.client(ctx)
	if err != nil {
		return HostChange{}, err
	}
	return c.AddHost(expectedRevision, host)
}

// RemoveHost removes one host at the listed configuration revision.
func (o *Operator) RemoveHost(ctx context.Context, expectedRevision, name string) (HostChange, error) {
	c, err := o.client(ctx)
	if err != nil {
		return HostChange{}, err
	}
	return c.RemoveHost(expectedRevision, name)
}

// Keys reads the local keys of this account.
func (o *Operator) Keys(ctx context.Context) (KeyList, error) {
	c, err := o.client(ctx)
	if err != nil {
		return KeyList{}, err
	}
	return c.Keys()
}

// IssueKey mints a local key for program; the reply carries it once.
func (o *Operator) IssueKey(ctx context.Context, program, credential string) (KeyIssued, error) {
	c, err := o.client(ctx)
	if err != nil {
		return KeyIssued{}, err
	}
	return c.IssueKey(program, credential)
}

// RevokeKey destroys the active local key of program.
func (o *Operator) RevokeKey(ctx context.Context, program string) (KeyRevoked, error) {
	c, err := o.client(ctx)
	if err != nil {
		return KeyRevoked{}, err
	}
	return c.RevokeKey(program)
}

// Audit reads retained decisions from sequence cursor.
func (o *Operator) Audit(ctx context.Context, cursor, maxEntries int64) (AuditPage, error) {
	c, err := o.client(ctx)
	if err != nil {
		return AuditPage{}, err
	}
	page, err := c.Audit(cursor, maxEntries)
	if err == nil && page.Outcome == wire.AuditOutcomePage {
		ok := int64(len(page.Entries)) <= maxEntries
		for i, e := range page.Entries {
			ok = ok && e.Sequence == cursor+int64(i)
		}
		err = check(ok && page.Next == cursor+int64(len(page.Entries)))
	}
	return page, err
}

// Gateway reads the gateway window setting and whether the window listens.
func (o *Operator) Gateway(ctx context.Context) (GatewayState, error) {
	c, err := o.client(ctx)
	if err != nil {
		return GatewayState{}, err
	}
	return c.Gateway()
}

// SetGateway opens or closes the gateway window at the listed setting revision
// and keeps the setting across restarts.
func (o *Operator) SetGateway(ctx context.Context, expectedRevision string, open bool, address string) (GatewayChange, error) {
	c, err := o.client(ctx)
	if err != nil {
		return GatewayChange{}, err
	}
	return c.SetGateway(expectedRevision, open, address)
}
