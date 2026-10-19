package sdcpp

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Addendum finding 2: the opening tag is refused in every spelling the
// folding covers, wherever it stands in the text.
func TestDirectiveIsRefusedInEveryCoveredSpelling(t *testing.T) {
	for _, text := range []string{
		`<sd_cpp_extra_args>{"lora":[{"path":"\\\\host\\share\\x.ckpt"}]}</sd_cpp_extra_args>`,
		"a lighthouse <sd_cpp_extra_args>{}</sd_cpp_extra_args>",
		"<SD_CPP_EXTRA_ARGS>{}",
		"<Sd_Cpp_Extra_Args",
		"\uFF1Csd_cpp_extra_args\uFF1E{}",                    // full-width < and >
		"\uFE64sd_cpp_extra_args>{}",                         // small less-than
		"<sd\uFF3Fcpp\uFE4Dextra\uFE4Fargs>",                 // full-width and small low lines
		"<\uFF53\uFF44_cpp_extra_args>",                      // full-width letters
		"<sd_\u200Bcpp_\u200Dextra_\u00ADargs>",              // zero-width and soft hyphen
		"<\u202Esd_cpp_extra_args\u202C>",                    // bidirectional override
		"<s\u0301d_cpp_extra_args\uFE0F>",                    // combining mark, variation selector
		"<\u017Fd_cpp_extra_args>",                           // long s
		"<sd_cpp_extra_arg\u017F>",                           // long s at the end
		"<sd_cpp_extra_args" + strings.Repeat(" ", 3) + "{}", // no closing bracket
		"<\U000E0073sd_cpp_extra_args>",                      // tag character inside
	} {
		if !HasDirective(text) {
			t.Fatalf("%q not refused", text)
		}
		err := CheckText("prompt", text)
		if !errors.Is(err, ErrEngineDirective) || !strings.Contains(err.Error(), "prompt") || Reason(err) != ReasonEngineDirective {
			t.Fatalf("%q: %v", text, err)
		}
		if strings.Contains(err.Error(), "lora") {
			t.Fatalf("error repeats the text: %v", err)
		}
	}
	for _, text := range []string{
		"a lighthouse at dusk", "sd_cpp_extra_args without its bracket", "< sd_cpp_extra_args>", "<sd-cpp-extra-args>",
		"<sd_cpp_extra>", "&lt;sd_cpp_extra_args&gt;", "", "<lora:style:1>",
	} {
		if HasDirective(text) || CheckText("prompt", text) != nil {
			t.Fatalf("%q refused", text)
		}
	}
}

// Addendum finding V1: width and height stay within 64..4096 and the pixel
// count within 2048 × 2048, so width × height × 4 stays far below 2^31.
func TestSizeBound(t *testing.T) {
	for _, size := range [][2]int{{64, 64}, {512, 512}, {1024, 1024}, {1792, 1024}, {2048, 2048}, {4096, 1024}, {1024, 4096}} {
		if err := CheckSize(size[0], size[1]); err != nil {
			t.Fatalf("%v refused: %v", size, err)
		}
		if err := CheckOpenAISize(fmt.Sprintf("%dx%d", size[0], size[1])); err != nil {
			t.Fatalf("%v word refused: %v", size, err)
		}
	}
	for _, size := range [][2]int{{63, 512}, {512, 0}, {-8, 512}, {4104, 512}, {2056, 2048}, {4096, 4096}, {65536, 21846}} {
		err := CheckSize(size[0], size[1])
		if !errors.Is(err, ErrImageSize) || Reason(err) != ReasonImageSize {
			t.Fatalf("%v: %v", size, err)
		}
	}
	if err := CheckOpenAISize("auto"); err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"", "x", "1024", "1024x", "x1024", "1024X1024", "+1024x1024", "1024x1024x3", " 1024x1024", "65536x21846", "0x0", "100000x8", "1e3x1e3"} {
		if err := CheckOpenAISize(word); !errors.Is(err, ErrImageSize) {
			t.Fatalf("%q: %v", word, err)
		}
	}
	if Reason(errors.New("other")) != "" || Reason(nil) != "" {
		t.Fatal("unrelated error has a reason")
	}
}
