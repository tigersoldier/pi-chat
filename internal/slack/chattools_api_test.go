package slack

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestRepliesPageUsesOneBoundedRequest(t *testing.T) {
	var calls atomic.Int32
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		got = r.PostForm
		_, _ = io.WriteString(w, `{"ok":true,"messages":[{"ts":"1.2","user":"U1","text":"hello"}],"has_more":true}`)
	}))
	defer srv.Close()
	api := NewAPI("xoxb-test", discardLogger(), WithBaseURL(srv.URL))

	page, err := api.RepliesPage(t.Context(), "C1", "1.1", "1.2", "1.3", 25, true)
	if err != nil {
		t.Fatalf("RepliesPage: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("conversations.replies requests = %d, want one", calls.Load())
	}
	if got.Get("channel") != "C1" || got.Get("ts") != "1.1" || got.Get("oldest") != "1.2" ||
		got.Get("latest") != "1.3" || got.Get("limit") != "25" || got.Get("inclusive") != "true" {
		t.Errorf("request parameters = %v", got)
	}
	if len(page.Messages) != 1 || page.Messages[0].Text != "hello" || !page.HasMore {
		t.Errorf("page = %+v", page)
	}
}

func TestUserProfileDecodesOnlyMinimalFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"user":{"id":"U1","name":"avery","real_name":"Avery Q","profile":{"display_name":"Avery","real_name":"Avery Q","title":"Engineer","email":"private@example.test"}}}`)
	}))
	defer srv.Close()
	api := NewAPI("xoxb-test", discardLogger(), WithBaseURL(srv.URL))

	got, err := api.UserProfile(t.Context(), "U1")
	if err != nil {
		t.Fatalf("UserProfile: %v", err)
	}
	if got != (UserProfile{ID: "U1", DisplayName: "Avery", Title: "Engineer"}) {
		t.Errorf("profile = %+v", got)
	}
}

func TestTranscriptAndChatToolShareTheUserProfileCache(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"user":{"id":"U1","profile":{"display_name":"Avery","title":"Engineer"}}}`)
	}))
	defer srv.Close()
	api := NewAPI("xoxb-test", discardLogger(), WithBaseURL(srv.URL))

	name, err := api.UserInfo(t.Context(), "U1")
	if err != nil || name != "Avery" {
		t.Fatalf("UserInfo = %q, %v", name, err)
	}
	profile, err := api.UserProfile(t.Context(), "U1")
	if err != nil || profile.Title != "Engineer" {
		t.Fatalf("UserProfile = %+v, %v", profile, err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("users.info requests = %d, want one shared lookup", got)
	}
}

func TestConversationInfoReturnsSelfDMIdentity(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		got = r.PostForm
		_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"D1","is_im":true,"user":"U1"}}`)
	}))
	defer srv.Close()
	api := NewAPI("xoxp-test", discardLogger(), WithBaseURL(srv.URL))

	info, err := api.ConversationInfo(t.Context(), "D1")
	if err != nil {
		t.Fatalf("ConversationInfo: %v", err)
	}
	if info != (ConversationInfo{ID: "D1", UserID: "U1", IsIM: true}) {
		t.Errorf("conversation info = %+v", info)
	}
	if got.Get("channel") != "D1" {
		t.Errorf("conversations.info channel = %q, want D1", got.Get("channel"))
	}
}
