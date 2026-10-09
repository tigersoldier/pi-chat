package slack

import (
	"strings"
	"testing"
)

// TestValidateUserToken covers the ways a credential can be the wrong kind. Each
// message has to name the fix, because the failure it replaces — invalid_auth —
// names nothing, and the person is looking at a file they pasted into.
func TestValidateUserToken(t *testing.T) {
	cases := []struct {
		name, token, want string
	}{
		{"a user token", "xoxp-1234567890-1234567890-abcdef", ""},
		{"whitespace around it", "  xoxp-123-456-abc\n", ""},
		{"empty", "", "empty"},
		{"a bot token", "xoxb-123-456-secret", "bot token"},
		{"a session token", "xoxc-123-456-secret", "session token"},
		{"an app-level token", "xapp-1-A123-secret", "app-level"},
		{"a rotating access token", "xoxe.xoxp-1-secret", "rotation"},
		{"a rotating refresh token", "xoxe-1-secret", "rotation"},
		{"something else", "hunter2-hunter2", "does not look like"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUserToken(tc.token)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("ValidateUserToken(%q) = %v, want nil", tc.token, err)
			case tc.want == "":
				return
			case err == nil:
				t.Fatalf("ValidateUserToken(%q) = nil, want an error mentioning %q", tc.token, tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestValidateUserTokenNeverEchoesTheToken is the property that makes the
// message safe to log: a diagnostic that quotes the credential it rejected is a
// credential leak with a helpful tone.
func TestValidateUserTokenNeverEchoesTheToken(t *testing.T) {
	for _, token := range []string{
		"xoxb-1111111111-2222222222-theSecretPart",
		"xoxc-1111111111-2222222222-theSecretPart",
		"xapp-1-A1111111-2222222222-theSecretPart",
		"xoxe.xoxp-1-theSecretPart",
		"xoxp-1111111111-2222222222-theSecretPart",
		"not-a-token-theSecretPart",
	} {
		if err := ValidateUserToken(token); err != nil && strings.Contains(err.Error(), "theSecretPart") {
			t.Errorf("the error for %q echoed the credential: %v", token[:8]+"…", err)
		}
	}
}
