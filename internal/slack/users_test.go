package slack

import (
	"context"
	"testing"
)

// TestLabelerAsksOncePerUser pins the cache: a conversation with the same person
// in it four times must not be four Web API calls.
func TestLabelerAsksOncePerUser(t *testing.T) {
	stub := &stubSlack{bodies: func(method string, _ map[string]any) string {
		if method == "users.info" {
			return `{"ok":true,"user":{"name":"alice","profile":{"display_name":"Alice"}}}`
		}
		return ""
	}}
	l := newLabeler(newStubAPI(t, stub), discardLogger())

	for range 4 {
		if name := l.name(context.Background(), "U2"); name != "Alice" {
			t.Fatalf("name = %q, want Alice", name)
		}
	}
	if n := callsTo(stub, "users.info"); n != 1 {
		t.Errorf("users.info requests = %d, want 1", n)
	}
}

// TestLabelerRemembersAUserThatDoesNotExist is the negative cache: a departed
// account would otherwise be looked up on every turn forever.
func TestLabelerRemembersAUserThatDoesNotExist(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"users.info": "user_not_found"}}
	l := newLabeler(newStubAPI(t, stub), discardLogger())

	for range 3 {
		if name := l.name(context.Background(), "U9"); name != "" {
			t.Fatalf("name = %q, want none", name)
		}
	}
	if n := callsTo(stub, "users.info"); n != 1 {
		t.Errorf("users.info requests = %d, want 1", n)
	}
}

// TestLabelerRetriesATransientFailure is the other half of the same decision: a
// failure that may pass must not be cached as if it were an answer.
func TestLabelerRetriesATransientFailure(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"users.info": "internal_error"}}
	l := newLabeler(newStubAPI(t, stub), discardLogger())

	for range 2 {
		if name := l.name(context.Background(), "U2"); name != "" {
			t.Fatalf("name = %q, want none while the lookup fails", name)
		}
	}
	if n := callsTo(stub, "users.info"); n != 2 {
		t.Errorf("users.info requests = %d, want 2: a transient failure is retried", n)
	}
}

// TestLabelerStopsAskingWhenTheScopeIsMissing is the latch: an install without
// `users:read` answers `missing_scope` for every user, and the answer will not
// change while this process lives.
func TestLabelerStopsAskingWhenTheScopeIsMissing(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"users.info": "missing_scope"}}
	l := newLabeler(newStubAPI(t, stub), discardLogger())

	for _, user := range []string{"U2", "U3", "U4"} {
		if name := l.name(context.Background(), user); name != "" {
			t.Fatalf("name of %s = %q, want none", user, name)
		}
	}
	if n := callsTo(stub, "users.info"); n != 1 {
		t.Errorf("users.info requests = %d, want 1: the refusal is the install's", n)
	}
}

// TestUserInfoPrefersTheChosenName pins which of a profile's several names is
// the one people recognize in a transcript.
func TestUserInfoPrefersTheChosenName(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "display name wins",
			body: `{"ok":true,"user":{"name":"alice","real_name":"Alice A","profile":{"display_name":"Ali","real_name":"Alice A"}}}`,
			want: "Ali",
		},
		{
			name: "then the profile's real name",
			body: `{"ok":true,"user":{"name":"alice","real_name":"Alice A","profile":{"display_name":"","real_name":"Alice A"}}}`,
			want: "Alice A",
		},
		{
			name: "then the account name",
			body: `{"ok":true,"user":{"name":"alice"}}`,
			want: "alice",
		},
		{
			name: "and blank is not a name",
			body: `{"ok":true,"user":{"name":"  "}}`,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubSlack{bodies: func(method string, _ map[string]any) string {
				if method == "users.info" {
					return tc.body
				}
				return ""
			}}
			got, err := newStubAPI(t, stub).UserInfo(context.Background(), "U2")
			if err != nil {
				t.Fatalf("UserInfo: %v", err)
			}
			if got != tc.want {
				t.Errorf("UserInfo = %q, want %q", got, tc.want)
			}
		})
	}
}
