package imagemask

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// contract is an [INF-I1] mask: fully transparent pixels mark the area to
// change, whatever their colour; kept pixels vary in colour and partial
// transparency, the cases a luminance reader misreads.
func contract(t *testing.T, w, h int, change func(x, y int) bool) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			switch {
			case change(x, y):
				m.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 40), A: 0})
			case x%2 == 0:
				m.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
			default:
				m.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 1})
			}
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func change(x, y int) bool { return (x+y)%3 == 0 }

// Each form keeps the contract's area: Luminance is white exactly there and
// black elsewhere; Alpha keeps alpha 0 exactly there and makes those pixels
// white and every other pixel opaque black, so an alpha reader and a
// luminance reader both read the same area.
func TestFormsKeepTheContractArea(t *testing.T) {
	mask := contract(t, 6, 4, change)
	for _, form := range []Form{Luminance, Alpha} {
		out, w, h, err := Convert(form, mask)
		if err != nil || w != 6 || h != 4 {
			t.Fatalf("form %d: %dx%d %v", form, w, h, err)
		}
		decoded, err := png.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < 4; y++ {
			for x := 0; x < 6; x++ {
				// A luminance reader (stb with one channel) weighs the
				// stored colour and drops alpha.
				stored := color.NRGBAModel.Convert(decoded.At(x, y)).(color.NRGBA)
				gray := uint8((int(stored.R) + int(stored.G) + int(stored.B)) / 3)
				alpha := stored.A
				wantGray := uint8(0)
				if change(x, y) {
					wantGray = 255
				}
				if gray != wantGray {
					t.Fatalf("form %d pixel %d,%d luminance %d", form, x, y, gray)
				}
				if form == Alpha && (alpha == 0) != change(x, y) {
					t.Fatalf("alpha form pixel %d,%d alpha %d", x, y, alpha)
				}
			}
		}
		if _, ok := decoded.(*image.Gray); ok != (form == Luminance) {
			t.Fatalf("form %d decoded as %T", form, decoded)
		}
	}
}

// The alpha form's colours are part of the form: white and transparent on
// the area to change, opaque black elsewhere, exactly.
func TestAlphaFormColours(t *testing.T) {
	out, _, _, err := Convert(Alpha, contract(t, 6, 4, change))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 6; x++ {
			got := color.NRGBAModel.Convert(decoded.At(x, y)).(color.NRGBA)
			want := color.NRGBA{A: 255}
			if change(x, y) {
				want = color.NRGBA{R: 255, G: 255, B: 255}
			}
			if got != want {
				t.Fatalf("pixel %d,%d = %+v, want %+v", x, y, got, want)
			}
		}
	}
}

// Inspect reads a contract mask's size and how many pixels it marks to
// change; an all-opaque mask marks none.
func TestInspectCountsTheAreaToChange(t *testing.T) {
	w, h, changed, err := Inspect(contract(t, 6, 4, change))
	want := 0
	for y := 0; y < 4; y++ {
		for x := 0; x < 6; x++ {
			if change(x, y) {
				want++
			}
		}
	}
	if err != nil || w != 6 || h != 4 || changed != want {
		t.Fatalf("inspect %dx%d %d changed, want %d: %v", w, h, changed, want, err)
	}
	if _, _, changed, err := Inspect(contract(t, 6, 4, func(int, int) bool { return false })); err != nil || changed != 0 {
		t.Fatalf("an opaque mask changes %d: %v", changed, err)
	}
	if _, _, _, err := Inspect([]byte("\x89PNG\r\n\x1a\n")); !errors.Is(err, ErrMask) {
		t.Fatalf("a truncated mask: %v", err)
	}
}

func TestConvertRefusesWhatIsNoMask(t *testing.T) {
	for name, data := range map[string][]byte{"empty": nil, "jpeg": []byte("\xff\xd8\xff\xe0\x00\x10JFIF"), "header only": contract(t, 2, 2, change)[:40]} {
		if _, _, _, err := Convert(Luminance, data); !errors.Is(err, ErrMask) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, _, _, err := Convert(Form(9), contract(t, 2, 2, change)); !errors.Is(err, ErrMask) {
		t.Fatalf("unknown form: %v", err)
	}
}
