package wavfile

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	for _, f := range []Format{{1, 24000}, {2, 16000}, {1, MinRate}, {2, MaxRate}} {
		wav, err := Encode(f, pcm)
		if err != nil || len(wav) != HeaderBytes+len(pcm) {
			t.Fatalf("%+v: %d bytes, %v", f, len(wav), err)
		}
		got, samples, err := Decode(wav, 1<<20)
		if err != nil || got != f || !bytes.Equal(samples, pcm) {
			t.Fatalf("%+v: %+v % x %v", f, got, samples, err)
		}
	}
}

func TestHeaderRefusesOtherShapes(t *testing.T) {
	for _, c := range []struct {
		f Format
		n int
	}{{Format{3, 24000}, 6}, {Format{1, 7999}, 2}, {Format{1, 48001}, 2}, {Format{1, 24000}, 3}, {Format{2, 24000}, 2}, {Format{1, 24000}, -2}} {
		if _, err := Header(c.f, c.n); !errors.Is(err, ErrFormat) {
			t.Fatalf("%+v %d: %v", c.f, c.n, err)
		}
	}
}

func TestDecodeRefusesOverCap(t *testing.T) {
	wav, err := Encode(Format{1, 16000}, make([]byte, 100))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(wav, len(wav)-1); !errors.Is(err, ErrFormat) {
		t.Fatal(err)
	}
}
