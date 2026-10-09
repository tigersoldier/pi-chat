package config

import (
	"strings"
	"testing"
	"time"
)

// TestSelfDMSurfaceLoadsAndNormalizes is the shape the daemon depends on: a
// bare workspace host becomes the API base, the interval parses, and the app
// surface's token paths stop being required once it is off.
func TestSelfDMSurfaceLoadsAndNormalizes(t *testing.T) {
	home(t)
	path := writeConfig(t, strings.Join([]string{
		"[slack]",
		"enabled = false",
		"[slack.self_dm]",
		"enabled = true",
		"workspace_url = \"acme.slack.com\"",
		"channel_id = \"D0123ABCD\"",
		"poll_interval = \"2s\"",
		"xoxc_file = \"/tmp/xoxc\"",
		"xoxd_file = \"/tmp/xoxd\"",
		"[slack.access]",
		"allowed_users = [\"U1\"]",
		"",
	}, "\n"))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Slack.AppEnabled() {
		t.Error("slack.enabled = false must turn the app surface off")
	}
	if !cfg.Slack.SelfDM.Enabled {
		t.Error("slack.self_dm.enabled = true must turn the self-DM surface on")
	}
	if want := "https://acme.slack.com/api/"; cfg.Slack.SelfDM.APIBase() != want {
		t.Errorf("APIBase = %q, want %q", cfg.Slack.SelfDM.APIBase(), want)
	}
	if got := cfg.Slack.SelfDM.PollEvery(); got != 2*time.Second {
		t.Errorf("PollEvery = %v, want 2s", got)
	}
}

// TestAppSurfaceIsOnByDefault pins the upgrade path: every configuration file
// written before the app surface was optional must keep working unchanged.
func TestAppSurfaceIsOnByDefault(t *testing.T) {
	home(t)
	cfg, err := Load(writeConfig(t, "[slack.access]\nallowed_users = [\"U1\"]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Slack.AppEnabled() {
		t.Error("an absent slack.enabled must mean the app surface is on")
	}
	if cfg.Slack.SelfDM.Enabled {
		t.Error("the self-DM surface must be off unless it is asked for")
	}
}

// TestEachSurfaceCanBeConfiguredAlone: turning the app off must not require
// inventing token paths, and turning it on must not require the self-DM.
func TestEachSurfaceCanBeConfiguredAlone(t *testing.T) {
	home(t)
	if _, err := Load(writeConfig(t, "[slack]\nenabled = false\n"+
		"[slack.self_dm]\nenabled = true\nworkspace_url = \"acme.slack.com\"\n"+
		"channel_id = \"D1\"\nxoxc_file = \"/tmp/x\"\nxoxd_file = \"/tmp/d\"\n")); err != nil {
		t.Errorf("self-DM alone must validate: %v", err)
	}
	if _, err := Load(writeConfig(t, "")); err != nil {
		t.Errorf("the app surface alone must validate: %v", err)
	}
}

// TestSelfDMJoinsTheSummary keeps `make check` honest about which surfaces are
// configured, without ever printing a secret.
func TestSelfDMJoinsTheSummary(t *testing.T) {
	home(t)
	cfg, err := Load(writeConfig(t, "[slack.self_dm]\nenabled = true\n"+
		"workspace_url = \"https://acme.slack.com\"\nchannel_id = \"D0123ABCD\"\n"+
		"xoxc_file = \"/tmp/xoxc\"\nxoxd_file = \"/tmp/xoxd\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	joined := strings.Join(cfg.Summary(), "\n")
	for _, want := range []string{"self-DM surface: on", "D0123ABCD", "https://acme.slack.com"} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary should mention %q, got:\n%s", want, joined)
		}
	}
}

// TestValidateRejectsBadSelfDMValues covers the ways a self-DM configuration
// can be wrong while looking complete.
func TestValidateRejectsBadSelfDMValues(t *testing.T) {
	home(t)
	const head = "[slack.self_dm]\nenabled = true\nxoxc_file = \"/tmp/x\"\nxoxd_file = \"/tmp/d\"\n"
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"no workspace",
			head + "channel_id = \"D1\"\n",
			"workspace_url",
		},
		{
			"no channel",
			head + "workspace_url = \"https://acme.slack.com\"\n",
			"channel_id",
		},
		{
			"channel that is not a DM",
			head + "workspace_url = \"https://acme.slack.com\"\nchannel_id = \"C123\"\n",
			"channel_id",
		},
		{
			"interval too small",
			head + "workspace_url = \"https://acme.slack.com\"\nchannel_id = \"D1\"\npoll_interval = \"200ms\"\n",
			"poll_interval",
		},
		{
			"interval that is not a duration",
			head + "workspace_url = \"https://acme.slack.com\"\nchannel_id = \"D1\"\npoll_interval = \"soon\"\n",
			"poll_interval",
		},
		{
			"relative credential path",
			"[slack.self_dm]\nenabled = true\nxoxc_file = \"xoxc\"\nxoxd_file = \"/tmp/d\"\n" +
				"workspace_url = \"https://acme.slack.com\"\nchannel_id = \"D1\"\n",
			"absolute",
		},
		{
			"workspace url without a scheme",
			head + "workspace_url = \"ftp://acme.slack.com\"\nchannel_id = \"D1\"\n",
			"workspace_url",
		},
		{
			"no surface at all",
			"[slack]\nenabled = false\n",
			"no surface is enabled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}
