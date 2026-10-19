//go:build !windows

package videoenc

// An Encoder writes one MP4 frame by frame; on this platform none starts.
type Encoder struct{}

// Create answers ErrNoSystemEncoder after checking its arguments: this
// platform has no system H.264 encoder this package binds, and none is
// bundled.
func Create(path string, o Options) (*Encoder, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	if err := checkPath(path); err != nil {
		return nil, err
	}
	return nil, ErrNoSystemEncoder
}

// Write answers ErrNoSystemEncoder.
func (e *Encoder) Write([]byte) error { return ErrNoSystemEncoder }

// Close answers ErrNoSystemEncoder.
func (e *Encoder) Close() (Encoded, error) { return Encoded{}, ErrNoSystemEncoder }

// Abort does nothing.
func (e *Encoder) Abort() {}
