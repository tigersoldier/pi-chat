package bot

import (
	"errors"
	"strings"
	"testing"
)

func TestPublicErrorKeepsTheCauseOutOfChat(t *testing.T) {
	cause := errors.New("store path /secret")
	err := &PublicError{Public: "I couldn't save the session", Cause: cause}

	if got := PublicMessage(err); got != "I couldn't save the session" {
		t.Fatalf("PublicMessage = %q, want only the public part", got)
	}
	if !strings.Contains(err.Error(), "store path /secret") {
		t.Fatalf("Error() = %q, want the cause kept for logs", err)
	}
	if !errors.Is(err, cause) {
		t.Fatal("PublicError must unwrap to its cause")
	}
}

func TestPublicMessageFallsBackToPlainErrors(t *testing.T) {
	if got := PublicMessage(errors.New("plain cause")); got != "plain cause" {
		t.Fatalf("PublicMessage = %q, want the existing text", got)
	}
}
