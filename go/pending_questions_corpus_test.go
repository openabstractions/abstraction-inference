package inference

import (
	"testing"

	"github.com/openabstractions/abstraction-asks/go/pendingquestioncases"
)

func TestPendingQuestionWordCorpus(t *testing.T) {
	cases := pendingquestioncases.Cases()
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := grantRefusalWords(tc.Reason); got != tc.Job {
				t.Fatalf("got %v, want %v", got, tc.Job)
			}
		})
	}
}
