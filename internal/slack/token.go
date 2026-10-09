package slack

import (
	"errors"
	"fmt"
	"strings"
)

// ValidateUserToken checks that a credential supplied for a user-token mode is
// really a Slack user token, and names the mistake when it is not. Getting this
// wrong is otherwise an `invalid_auth` a long way from its cause: an `xoxb-`
// pasted where an `xoxp-` belongs looks like a wrong password.
//
// The returned error never contains the token: these messages are logged and
// printed by `pi-chatd --check`, and a credential in a log is a leaked
// credential.
func ValidateUserToken(token string) error {
	value := strings.TrimSpace(token)
	switch {
	case value == "":
		return errors.New("the token file is empty")
	case strings.HasPrefix(value, "xoxp-"):
		return nil
	case strings.HasPrefix(value, "xoxe."), strings.HasPrefix(value, "xoxe-"):
		// `xoxe.xoxp-…` is an expiring access token and `xoxe-1-…` a refresh
		// token: both mean the app has token rotation enabled.
		return errors.New("this is an expiring token (xoxe…): the app has token rotation enabled, " +
			"and pi-chat does not refresh tokens — turn token rotation off in the app settings and " +
			"reinstall, then authorize again")
	case strings.HasPrefix(value, "xoxb-"):
		return errors.New("this is a bot token (xoxb-…); the self-DM needs a person's user token (xoxp-…)")
	case strings.HasPrefix(value, "xoxc-"):
		return errors.New("this is a browser session token (xoxc-…); " +
			"set slack.self_dm.auth = \"session\" to use it together with its d cookie")
	case strings.HasPrefix(value, "xapp-"):
		return errors.New("this is an app-level token (xapp-…), which only opens Socket Mode")
	default:
		return fmt.Errorf("this does not look like a Slack user token (xoxp-…): %s", describeToken(value))
	}
}

// describeToken says what a credential looks like without reproducing it. The
// first segment is the part that identifies the kind; the length distinguishes
// a truncated paste from a wrong one.
func describeToken(value string) string {
	kind := value
	if i := strings.IndexAny(kind, "-."); i > 0 {
		kind = kind[:i]
	}
	if len(kind) > 12 {
		kind = kind[:12]
	}
	return fmt.Sprintf("it starts with %q and is %d bytes long", kind, len(value))
}
