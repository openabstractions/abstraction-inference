// Mandatory pure request safety shared by mediation and engine adapters.
package mediationcontent

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ErrEngineDirective reports text that carries sd-server's
// `<sd_cpp_extra_args>` directive. sd-server's /v1/images/* and /sdapi/v1/*
// routes parse a JSON block between that tag and its closing tag in the
// prompt as generation parameters with no LoRA path check: a `lora` path
// there loads any readable file, including through the pickle parser, and a
// UNC path makes Windows send the account's NTLM credentials to that host.
var ErrEngineDirective = errors.New("sd.cpp: text carries the sd_cpp_extra_args engine directive")

// ErrImageSize reports an image size, requested or of an input image,
// beyond the reviewed bound.
var ErrImageSize = errors.New("sd.cpp: image size outside the reviewed bound")

// ErrRequestForm reports a request a provider refuses to forward toward
// sd-server for its form: a body that does not parse, a field or part
// outside the ones inference sends, a repeated or transfer-encoded part, or
// an input image whose header does not parse.
var ErrRequestForm = errors.New("sd.cpp: request form not forwarded")

// Reason words for the refusals (inference CONTRACT.md INF-SD1, INF-SD2,
// INF-SD3).
const (
	ReasonEngineDirective = "prompt:engine-directive"
	ReasonImageSize       = "size:engine-bound"
	ReasonRequestForm     = "request:engine-form"
)

// Size bounds. sd-server decodes each request image against the request's
// own width and height and allocates width × height × channels in C int
// arithmetic before resizing into it (addendum V1); a product above 2^31
// wraps and the resize overruns the heap. MinDimension and MaxDimension match
// inference's own size bound and the width and height range sd-server
// advertises in /sdcpp/v1/capabilities, and MaxPixels (2048 × 2048) keeps the
// product at most 16,777,216 bytes for four channels.
const (
	MinDimension = 64
	MaxDimension = 4096
	MaxPixels    = 2048 * 2048
)

// directive is the opening tag sd-server searches for, folded.
const directive = "<sd_cpp_extra_args"

// fold maps one rune to the form HasDirective compares, or reports that the
// rune is dropped. It covers the look-alikes Unicode compatibility
// normalisation (NFKC) turns into the tag's characters and the characters a
// later stage could strip from between them:
//   - the full-width forms U+FF01..U+FF5E, including ＜ (U+FF1C) and ＿
//     (U+FF3F), become their ASCII characters;
//   - the small less-than sign ﹤ (U+FE64) becomes <, and the dashed and wavy
//     low lines U+FE4D..U+FE4F become _;
//   - format characters (Unicode category Cf: soft hyphen, zero-width space
//     and joiners, bidirectional marks, word joiner, BOM, tag characters) and
//     non-spacing marks (category Mn: combining accents, variation
//     selectors) are dropped;
//   - letters are case-folded through upper case and back, which also turns
//     the long s ſ (U+017F) into s, the dotless ı (U+0131) into i and the
//     Kelvin sign (U+212A) into k.
//
// Other look-alikes, such as the mathematical alphanumeric letters, angle
// brackets 〈 and ‹, and HTML or percent escapes, are not folded.
func fold(r rune) (rune, bool) {
	switch {
	case r >= 0xFF01 && r <= 0xFF5E:
		r -= 0xFEE0
	case r == 0xFE64:
		r = '<'
	case r >= 0xFE4D && r <= 0xFE4F:
		r = '_'
	}
	if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Mn, r) {
		return 0, false
	}
	return unicode.ToLower(unicode.ToUpper(r)), true
}

// HasDirective reports whether text carries the opening tag of sd-server's
// engine directive, `<sd_cpp_extra_args`, in any letter case and after the
// folding described at fold.
func HasDirective(text string) bool {
	var folded strings.Builder
	folded.Grow(len(text))
	for _, r := range text {
		if f, keep := fold(r); keep {
			folded.WriteRune(f)
		}
	}
	return strings.Contains(folded.String(), directive)
}

// CheckText refuses text that carries the engine directive, wrapping
// ErrEngineDirective and naming the field, never the text.
func CheckText(field, text string) error {
	if HasDirective(text) {
		return fmt.Errorf("%w: %s", ErrEngineDirective, field)
	}
	return nil
}

// CheckSize refuses a width or height outside MinDimension..MaxDimension, or
// more than MaxPixels pixels, wrapping ErrImageSize.
func CheckSize(width, height int) error {
	if width < MinDimension || height < MinDimension || width > MaxDimension || height > MaxDimension || width*height > MaxPixels {
		return fmt.Errorf("%w: %dx%d", ErrImageSize, width, height)
	}
	return nil
}

// CheckOpenAISize checks an OpenAI-style size: "auto", which leaves the size
// to the engine's launch defaults, or `<width>x<height>` in decimal digits
// within CheckSize's bound. Every other spelling is refused.
func CheckOpenAISize(size string) error {
	if size == "auto" {
		return nil
	}
	w, h, ok := strings.Cut(size, "x")
	width, okW := decimal(w)
	height, okH := decimal(h)
	if !ok || !okW || !okH {
		return fmt.Errorf("%w: %q", ErrImageSize, size)
	}
	return CheckSize(width, height)
}

// decimal parses 1..5 ASCII digits.
func decimal(s string) (int, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Reason returns the mandatory image request-safety reason in err's chain.
func Reason(err error) string {
	switch {
	case errors.Is(err, ErrEngineDirective):
		return ReasonEngineDirective
	case errors.Is(err, ErrImageSize):
		return ReasonImageSize
	case errors.Is(err, ErrRequestForm):
		return ReasonRequestForm
	}
	return ""
}
