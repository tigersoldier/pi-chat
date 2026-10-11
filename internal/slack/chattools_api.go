package slack

import (
	"context"
	"strings"
	"time"
)

const (
	userProfileCacheTTL = 10 * time.Minute
	maxUserProfileCache = 1024
)

type userProfileCacheEntry struct {
	profile UserProfile
	expires time.Time
}

// ConversationInfo is the small part of Slack's channel object needed to
// distinguish the authenticated user's own DM from a DM with another person.
type ConversationInfo struct {
	ID     string
	UserID string
	IsIM   bool
}

// ConversationInfo fetches the channel identity. For a one-to-one DM, Slack's
// `user` field names the other participant; it equals the authenticated user ID
// only for that user's self-DM.
func (a *API) ConversationInfo(ctx context.Context, channelID string) (ConversationInfo, error) {
	var out struct {
		Channel struct {
			ID   string `json:"id"`
			User string `json:"user"`
			IsIM bool   `json:"is_im"`
		} `json:"channel"`
	}
	if err := a.call(ctx, "conversations.info", map[string]any{"channel": channelID}, &out); err != nil {
		return ConversationInfo{}, err
	}
	return ConversationInfo{ID: strings.TrimSpace(out.Channel.ID), UserID: strings.TrimSpace(out.Channel.User), IsIM: out.Channel.IsIM}, nil
}

// BoundedRepliesPage is one conversations.replies page without cursor chasing.
// Chat tools need a strict page bound; bot transcripts use Replies instead.
type BoundedRepliesPage struct {
	Messages []Message
	HasMore  bool
}

// RepliesPage fetches one bounded page from the specified thread. Slack returns
// messages oldest-first for this endpoint. Bounds are optional; inclusive is
// only sent when true.
func (a *API) RepliesPage(ctx context.Context, channel, threadTS, oldest, latest string, limit int, inclusive bool) (BoundedRepliesPage, error) {
	if limit <= 0 || limit > replyPageSize {
		limit = replyPageSize
	}
	params := map[string]any{"channel": channel, "ts": threadTS, "limit": limit}
	if oldest != "" {
		params["oldest"] = oldest
	}
	if latest != "" {
		params["latest"] = latest
	}
	if inclusive {
		params["inclusive"] = true
	}
	var page struct {
		Messages []Message `json:"messages"`
		HasMore  bool      `json:"has_more"`
	}
	if err := a.call(ctx, "conversations.replies", params, &page); err != nil {
		return BoundedRepliesPage{}, err
	}
	return BoundedRepliesPage{Messages: page.Messages, HasMore: page.HasMore}, nil
}

// UserProfile contains the only profile fields returned by chat tools. Email,
// phone and other profile fields are deliberately never decoded or exposed.
type UserProfile struct {
	ID          string
	DisplayName string
	Title       string
}

// UserProfile reads a minimal profile through a shared cache so transcript
// labeling and explicit profile tools reuse the same users.info response.
func (a *API) UserProfile(ctx context.Context, userID string) (UserProfile, error) {
	now := time.Now()
	a.profileMu.Lock()
	if entry, ok := a.profiles[userID]; ok && now.Before(entry.expires) {
		a.profileMu.Unlock()
		return entry.profile, nil
	}
	delete(a.profiles, userID)
	denied := a.profileDenied
	a.profileMu.Unlock()
	if denied {
		return UserProfile{}, &Error{Method: "users.info", Code: "missing_scope"}
	}

	var out struct {
		User struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
				Title       string `json:"title"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := a.call(ctx, "users.info", map[string]any{"user": userID}, &out); err != nil {
		if capabilityUnavailable(err) {
			a.profileMu.Lock()
			a.profileDenied = true
			a.profileMu.Unlock()
		}
		return UserProfile{}, err
	}
	profile := UserProfile{
		ID:          strings.TrimSpace(out.User.ID),
		DisplayName: firstNonEmpty(out.User.Profile.DisplayName, out.User.Profile.RealName, out.User.RealName, out.User.Name),
		Title:       strings.TrimSpace(out.User.Profile.Title),
	}
	a.profileMu.Lock()
	if a.profiles == nil {
		a.profiles = make(map[string]userProfileCacheEntry)
	}
	if len(a.profiles) >= maxUserProfileCache {
		oldestID := ""
		var oldestExpiry time.Time
		for id, cached := range a.profiles {
			if !now.Before(cached.expires) {
				delete(a.profiles, id)
				continue
			}
			if oldestID == "" || cached.expires.Before(oldestExpiry) {
				oldestID, oldestExpiry = id, cached.expires
			}
		}
		if len(a.profiles) >= maxUserProfileCache && oldestID != "" {
			delete(a.profiles, oldestID)
		}
	}
	a.profiles[userID] = userProfileCacheEntry{profile: profile, expires: now.Add(userProfileCacheTTL)}
	a.profileMu.Unlock()
	return profile, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
