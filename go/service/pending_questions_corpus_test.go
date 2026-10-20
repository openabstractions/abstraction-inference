package service

import (
	"testing"

	"github.com/openabstractions/abstraction-asks/go/pendingquestioncases"
)

func TestPendingQuestionWordCorpus(t *testing.T) {
	cases := pendingquestioncases.Cases()
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := pendingPermissionRefusal("not_permitted", tc.Reason); got != tc.Service {
				t.Fatalf("got %v, want %v", got, tc.Service)
			}
		})
	}
	for _, tc := range cases {
		if tc.Name == "producer" && "rights:not_granted "+questionWords([]string{"a", "b", "a", "", "c d", "c\td", "c\rd", "c\nd"}) != tc.Reason {
			t.Fatal("producer disagrees with shared refusal corpus")
		}
	}
}
