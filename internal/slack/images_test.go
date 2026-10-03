package slack

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

type imageTransport func(*http.Request) (*http.Response, error)

func (f imageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func imageResponse(code int, data []byte) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data))}
}

func imageAPI(t *testing.T, metadata string, download imageTransport) (*API, *stubSlack) {
	t.Helper()
	stub := &stubSlack{bodies: func(method string, params map[string]any) string {
		if method == "files.info" {
			if params["file"] != "F1" {
				t.Errorf("files.info file = %v, want F1", params["file"])
			}
			return `{"ok":true,"file":` + metadata + `}`
		}
		return ""
	}}
	api := newStubAPI(t, stub)
	api.http.Transport = imageTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/files.info") {
			return http.DefaultTransport.RoundTrip(r)
		}
		return download(r)
	})
	return api, stub
}

func TestReadImageAuthenticatesAndSendsActualBytes(t *testing.T) {
	data := tinyPNG(t)
	calls := 0
	api, stub := imageAPI(t, `{"mimetype":"image/png","url_private_download":"https://files.slack.com/private/image"}`,
		func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer xoxb-test" {
				t.Errorf("download = %s, auth %q", r.Method, r.Header.Get("Authorization"))
			}
			return imageResponse(http.StatusOK, data), nil
		})
	img, err := api.ReadImage(context.Background(), "F1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil || !bytes.Equal(got, data) || img.Type != "image" || img.MIMEType != "image/png" {
		t.Fatalf("image content = %+v, decode error %v", img, err)
	}
	if calls != 1 || callsTo(stub, "files.info") != 1 {
		t.Fatalf("download calls = %d, API calls = %v", calls, stub.methods())
	}
}

func TestReadImageUsesURLPrivateFallbackAndSniffsMIME(t *testing.T) {
	api, _ := imageAPI(t, `{"mimetype":"image/jpeg","url_private":"https://files.slack.com/image"}`,
		func(*http.Request) (*http.Response, error) { return imageResponse(200, tinyPNG(t)), nil })
	img, err := api.ReadImage(context.Background(), "F1")
	if err != nil || img.MIMEType != "image/png" {
		t.Fatalf("image = %+v, err %v: use the bytes, not Slack's label", img, err)
	}
}

func TestReadImageRejectsUnavailableUnsupportedAndOversizedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, want string
		response             *http.Response
		wantDownloads        int
	}{
		{"PDF", `{"mimetype":"application/pdf"}`, "unsupported attachment", nil, 0},
		{"oversized metadata", fmt.Sprintf(`{"mimetype":"image/png","size":%d}`, maxImageBytes+1), "5 MiB", nil, 0},
		{"missing URL", `{}`, "trusted Slack", nil, 0},
		{"untrusted URL", `{"url_private":"https://attacker.example/image"}`, "trusted Slack", nil, 0},
		{"HTTP URL", `{"url_private":"http://files.slack.com/image"}`, "trusted Slack", nil, 0},
		{"login page", `{"url_private":"https://files.slack.com/image"}`, "not a supported image", imageResponse(200, []byte("<html>Sign in</html>")), 1},
		{"denied", `{"url_private":"https://files.slack.com/image"}`, "HTTP 403", imageResponse(403, nil), 1},
		{"oversized body", `{"url_private":"https://files.slack.com/image"}`, "5 MiB", imageResponse(200, bytes.Repeat([]byte{'x'}, maxImageBytes+1)), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			api, _ := imageAPI(t, tc.metadata, func(*http.Request) (*http.Response, error) {
				calls++
				return tc.response, nil
			})
			_, err := api.ReadImage(context.Background(), "F1")
			if err == nil || !strings.Contains(err.Error(), tc.want) || calls != tc.wantDownloads {
				t.Fatalf("err = %v, downloads = %d", err, calls)
			}
		})
	}
}

func TestReadImageBoundsAChunkedBody(t *testing.T) {
	api, _ := imageAPI(t, `{"url_private":"https://files.slack.com/image"}`, func(*http.Request) (*http.Response, error) {
		r := imageResponse(200, bytes.Repeat([]byte{'x'}, maxImageBytes+1))
		r.ContentLength = -1
		return r, nil
	})
	if _, err := api.ReadImage(context.Background(), "F1"); err == nil || !strings.Contains(err.Error(), "5 MiB") {
		t.Fatalf("unbounded chunked download: %v", err)
	}
}

func TestReadImageDoesNotFollowUntrustedRedirectsOrLeakPrivateURLs(t *testing.T) {
	calls := 0
	api, _ := imageAPI(t, `{"url_private":"https://files.slack.com/secret-url"}`, func(*http.Request) (*http.Response, error) {
		calls++
		r := imageResponse(302, nil)
		r.Header.Set("Location", "https://attacker.example/steal-token")
		return r, nil
	})
	_, err := api.ReadImage(context.Background(), "F1")
	if err == nil || calls != 1 || strings.Contains(err.Error(), "secret-url") || strings.Contains(err.Error(), "steal-token") {
		t.Fatalf("err = %v, requests = %d", err, calls)
	}
}

func TestReadImageAllowsTrustedRedirectsWithAuthentication(t *testing.T) {
	calls := 0
	api, _ := imageAPI(t, `{"url_private":"https://files.slack.com/image"}`, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			t.Errorf("redirect lost auth: %q", r.Header.Get("Authorization"))
		}
		if calls == 1 {
			resp := imageResponse(302, nil)
			resp.Header.Set("Location", "https://files-origin.slack.com/image")
			return resp, nil
		}
		return imageResponse(200, tinyPNG(t)), nil
	})
	if _, err := api.ReadImage(context.Background(), "F1"); err != nil || calls != 2 {
		t.Fatalf("redirect: %v, requests %d", err, calls)
	}
}

func TestReadImageExplainsMissingFilesScope(t *testing.T) {
	api := newStubAPI(t, &stubSlack{failed: map[string]string{"files.info": "missing_scope"}})
	if _, err := api.ReadImage(context.Background(), "F1"); err == nil || !strings.Contains(err.Error(), "files:read") {
		t.Fatalf("missing_scope: %v", err)
	}
}

func TestReadImageHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	downloaded := false
	api, _ := imageAPI(t, `{"url_private":"https://files.slack.com/image"}`, func(r *http.Request) (*http.Response, error) {
		downloaded = true
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	if _, err := api.ReadImage(ctx, "F1"); !errors.Is(err, context.Canceled) || !downloaded {
		t.Fatalf("canceled read: %v, downloaded=%v", err, downloaded)
	}
}

func TestFileURLRestrictsHostsPortsAndCredentials(t *testing.T) {
	for _, raw := range []string{
		"https://files.slack.com.evil.example/image", "https://127.0.0.1/image",
		"https://files.slack.com:8443/image", "https://user:pass@files.slack.com/image",
		"https://api.slack.com/image", "file:///etc/passwd", "https://files.slack.com./image",
	} {
		u, err := url.Parse(raw)
		if err != nil || fileURL(u) {
			t.Errorf("unexpected trusted URL: %q, %v", raw, err)
		}
	}
}

func TestParseMessagePreservesImageOnlyFileShares(t *testing.T) {
	for _, event := range []string{
		`{"type":"app_mention","subtype":"file_share","user":"U1","text":"<@U0BOT>","ts":"1","channel":"C1","files":[{"id":"F1","name":"bug.png"}]}`,
		`{"type":"message","subtype":"file_share","channel_type":"im","user":"U1","ts":"1","channel":"D1","files":[{"id":"F1","name":"bug.png"}]}`,
	} {
		m, ok := parseMessage(eventsAPI(event), botUser)
		if !ok || m.Text != "" || len(m.Files) != 1 || m.Files[0] != (bot.Attachment{ID: "F1", Name: "bug.png"}) {
			t.Fatalf("file share parsed as %+v, ok=%v", m, ok)
		}
	}
}

func TestConversationPreservesImageOnlyFileShares(t *testing.T) {
	stub := &stubSlack{bodies: func(method string, _ map[string]any) string {
		if method == "conversations.replies" {
			return repliesBody(`{"ts":"1.1","subtype":"file_share","user":"U2","files":[{"id":"F1","title":"Screenshot"}]}`, false, "")
		}
		return ""
	}}
	p := testPlatformWithIdentity(t, stub, Identity{UserID: "U0"})
	said, err := p.Conversation(context.Background(), bot.Thread{Channel: "C1", ThreadTS: "1.1"}, "")
	if err != nil || len(said) != 1 || len(said[0].Files) != 1 || said[0].Files[0].Name != "Screenshot" {
		t.Fatalf("conversation lost the screenshot: %+v, %v", said, err)
	}
	if callsTo(stub, "files.info") != 0 {
		t.Fatal("conversation should not download before the core filters it")
	}
}
