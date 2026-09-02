package config

import "testing"

func TestDefaultSMTPPolicyPreservesCatchAll(t *testing.T) {
	t.Parallel()

	policy := NewSMTPPolicy()
	if got := policy.EvaluateRecipient(RecipientRequest{}).Action; got != RecipientAccept {
		t.Fatalf("recipient action = %v, want accept", got)
	}
	if got := policy.EvaluateMessage(nil).Action; got != MessageAccept {
		t.Fatalf("message action = %v, want accept", got)
	}
}

func TestDecisionZeroValuesFailClosed(t *testing.T) {
	t.Parallel()

	if got := (RecipientDecision{}).Action; got == RecipientAccept {
		t.Fatal("zero recipient decision accepts")
	}
	if got := (MessageDecision{}).Action; got == MessageAccept {
		t.Fatal("zero message decision accepts")
	}
}
