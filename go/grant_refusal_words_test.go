package inference

import "testing"

// A grant refusal that names its pending questions stays a grant refusal; a
// question word alone, or beside a word that is no grant refusal, is not one.
func TestGrantRefusalWordsAcceptPendingQuestionWords(t *testing.T) {
	for reason, want := range map[string]bool{
		"rights:not_granted":                            true,
		"rights:not_granted rights:question_pending:q1": true,
		"rights:not_granted content:read:forbidden rights:question_pending:q1 rights:question_pending:q2": true,
		"rights:question_pending:q1":                  false,
		"rights:not_granted rights:question_pending:": false,
		"rights:not_granted upstream:forbidden":       false,
		"":                                            false,
	} {
		if got := grantRefusalWords(reason); got != want {
			t.Errorf("%q: %v, want %v", reason, got, want)
		}
	}
}
