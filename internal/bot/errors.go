package bot

import (
	"errors"
	"strings"
)

// PublicError is an error whose PublicMessage is safe to show in chat while its
// Cause stays available to logs and to errors.Is/As. Chat adapters render
// PublicMessage instead of Error, so a gateway path, a store failure, or a
// provider detail can explain itself without leaking into Slack.
type PublicError struct {
	Public string
	Cause  error
}

func (e *PublicError) Error() string {
	if e == nil {
		return ""
	}
	public := strings.TrimSpace(e.Public)
	if e.Cause == nil || strings.TrimSpace(e.Cause.Error()) == "" {
		return public
	}
	if public == "" {
		return e.Cause.Error()
	}
	return public + ": " + e.Cause.Error()
}

func (e *PublicError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *PublicError) PublicMessage() string {
	if e == nil {
		return ""
	}
	return e.Public
}

// PublicMessage returns the message safe to show in chat for err. Errors that
// do not carry a public message keep their existing text, so user-facing errors
// that were already written for chat do not need to change.
func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	var public interface{ PublicMessage() string }
	if errors.As(err, &public) {
		if msg := strings.TrimSpace(public.PublicMessage()); msg != "" {
			return msg
		}
	}
	return err.Error()
}

func publicError(public string, cause error) error {
	return &PublicError{Public: public, Cause: cause}
}
