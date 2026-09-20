package inference

import (
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
)

func TestImageJobModeMapsByWireWord(t *testing.T) {
	for _, test := range []struct {
		job  jobwire.Mode
		want wire.ImageMode
	}{
		{jobwire.ModeGenerate, wire.ImageModeGenerate},
		{jobwire.ModeEdit, wire.ImageModeEdit},
		{jobwire.Mode(99), 0},
	} {
		if got := imageWireRequest(jobwire.ImageRequest{Mode: test.job}).Mode; got != test.want {
			t.Fatalf("mode %q mapped to %q, want %q", test.job.String(), got.String(), test.want.String())
		}
	}
}
