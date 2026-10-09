package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-gateway/protocol"
)

const maxImageBytes = 5 << 20

// File is the subset of Slack file metadata needed to identify and read images.
// Event payloads can contain only an ID (including delayed Slack Connect shares),
// so files.info remains authoritative for the URL and MIME type.
type File struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	MIMEType    string `json:"mimetype"`
	Size        int64  `json:"size"`
	URLDownload string `json:"url_private_download"`
	URLPrivate  string `json:"url_private"`
}

func attachments(files []File) []bot.Attachment {
	out := make([]bot.Attachment, 0, len(files))
	for _, f := range files {
		name := f.Name
		if name == "" {
			name = f.Title
		}
		out = append(out, bot.Attachment{ID: f.ID, Name: name})
	}
	return out
}

func supportedImage(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// fileURL prevents sending the bot token to arbitrary URLs or following a
// redirect into the local network. Restrict ports as well as names: Slack's
// private file URLs use HTTPS on the default port.
func fileURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	switch u.Hostname() {
	case "files.slack.com", "files-origin.slack.com":
		return true
	}
	return false
}

// ReadImage implements bot.ImageReader. Bytes live only long enough to become
// native pi image content; nothing is written into a project or the state DB.
func (p *Platform) ReadImage(ctx context.Context, file bot.Attachment) (protocol.ImageContent, error) {
	return p.api.ReadImage(ctx, file.ID)
}

func (a *API) ReadImage(ctx context.Context, id string) (protocol.ImageContent, error) {
	if id == "" {
		return protocol.ImageContent{}, errors.New("attachment has no file ID")
	}
	var out struct {
		File File `json:"file"`
	}
	if err := a.call(ctx, "files.info", map[string]any{"file": id}, &out); err != nil {
		if IsCode(err, "missing_scope") {
			return protocol.ImageContent{}, errors.New("Slack needs files:read; reinstall the app with that scope")
		}
		return protocol.ImageContent{}, err
	}
	f := out.File
	if f.MIMEType != "" && !supportedImage(f.MIMEType) {
		return protocol.ImageContent{}, errors.New("unsupported attachment; send PNG, JPEG, GIF or WebP")
	}
	if f.Size > maxImageBytes {
		return protocol.ImageContent{}, errors.New("image exceeds the 5 MiB limit")
	}
	rawURL := f.URLDownload
	if rawURL == "" {
		rawURL = f.URLPrivate
	}
	u, err := url.Parse(rawURL)
	if err != nil || !fileURL(u) {
		return protocol.ImageContent{}, errors.New("attachment has no trusted Slack download URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return protocol.ImageContent{}, errors.New("cannot build the image download request")
	}
	req.Header.Set("Authorization", "Bearer "+a.token)

	// Copy the client, not its transport: downloads share connection pooling but
	// must never change the redirect policy of concurrent Web API calls.
	client := *a.http
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !fileURL(req.URL) {
			return errors.New("untrusted image redirect")
		}
		// net/http may forward sensitive headers to subdomains; our smaller
		// allowlist makes this explicit rather than trusting that default.
		req.Header.Set("Authorization", "Bearer "+a.token)
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http errors include the private URL. Keep it out of logs/prompts.
		if ctx.Err() != nil {
			return protocol.ImageContent{}, ctx.Err()
		}
		return protocol.ImageContent{}, errors.New("Slack image download failed (network error or untrusted redirect)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return protocol.ImageContent{}, fmt.Errorf("Slack image download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxImageBytes {
		return protocol.ImageContent{}, errors.New("image exceeds the 5 MiB limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return protocol.ImageContent{}, errors.New("cannot read the Slack image download")
	}
	if len(data) > maxImageBytes {
		return protocol.ImageContent{}, errors.New("image exceeds the 5 MiB limit")
	}
	mime := http.DetectContentType(data)
	if !supportedImage(mime) {
		// Slack can return an HTML sign-in page with HTTP 200. Sending that to
		// the model as an image would hide the actual authentication failure.
		return protocol.ImageContent{}, errors.New("download is not a supported image (possibly a Slack sign-in page)")
	}
	return protocol.NewImage(data, mime), nil
}
