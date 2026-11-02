package bot

import (
	"errors"
	"fmt"
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

func TestPublicMessageHidesAnEmptyPublicText(t *testing.T) {
	err := &PublicError{Public: "  ", Cause: errors.New("store path /secret")}
	if got := PublicMessage(err); got != "the request failed" {
		t.Fatalf("PublicMessage = %q, want the generic fallback", got)
	}
	if strings.Contains(PublicMessage(err), "secret") {
		t.Fatal("an empty public text leaked the cause")
	}
}

func TestPublicMessageFindsAWrappedPublicError(t *testing.T) {
	err := fmt.Errorf("handling the turn: %w", &PublicError{Public: "the model request failed", Cause: errors.New("detail")})
	if got := PublicMessage(err); got != "the model request failed" {
		t.Fatalf("PublicMessage = %q, want the wrapped public text", got)
	}
}

func TestPublicMessageOfNilIsEmpty(t *testing.T) {
	if got := PublicMessage(nil); got != "" {
		t.Fatalf("PublicMessage(nil) = %q, want empty", got)
	}
}
