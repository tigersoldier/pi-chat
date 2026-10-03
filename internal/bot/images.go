package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tigersoldier/pi-gateway/protocol"
)

const (
	maxPromptImages = 4
	maxImageReads   = 8
	// 10 MiB of image bytes after base64 encoding, with ample room for text,
	// JSON and command framing under the gateway's 16 MiB frame limit.
	imageDataBudget = (10 << 20) / 3 * 4
	imageTimeout    = 15 * time.Second
	imagesTimeout   = 45 * time.Second
)

type observedFile struct {
	file   Attachment
	source string
}

type preparedPrompt struct {
	text     string
	images   []protocol.ImageContent
	warnings []string
}

// attachmentName is bounded and quoted, so a filename cannot introduce a fake
// author or a new line in the transcript. The raw name is never used as a path.
func attachmentName(f Attachment) string {
	name := f.Name
	if name == "" {
		name = f.ID
	}
	if len(name) > 160 {
		name = name[:160] + "…"
	}
	return strconv.Quote(name)
}

func attachmentSummary(files []Attachment) string {
	if len(files) == 0 {
		return ""
	}
	names := make([]string, 0, min(len(files), maxImageReads))
	for _, f := range files[:min(len(files), maxImageReads)] {
		names = append(names, attachmentName(f))
	}
	if len(files) > maxImageReads {
		names = append(names, fmt.Sprintf("%d more", len(files)-maxImageReads))
	}
	return " [attachments: " + strings.Join(names, ", ") + "]"
}

// preparePrompt loads only images that this turn will carry. The adapter owns
// the authenticated download; the core owns aggregate limits, dedupe and the
// distinction between somebody's conversation and the actual request.
func (th *thread) preparePrompt(ctx context.Context, m Message, obs observation) (preparedPrompt, error) {
	ctx, cancel := context.WithTimeout(ctx, imagesTimeout)
	defer cancel()
	out := preparedPrompt{}
	reader, canRead := th.b.plat.(ImageReader)
	files := make([]observedFile, 0, len(m.Files)+len(obs.files))
	for _, f := range m.Files {
		files = append(files, observedFile{file: f, source: "request"})
	}
	files = append(files, obs.files...)

	seen := make(map[string]bool)
	requestNotes, contextNotes := []string{}, []string{}
	budget, reads, triggerImages := imageDataBudget, 0, 0
	for _, f := range files {
		if f.file.ID != "" && seen[f.file.ID] {
			continue
		}
		seen[f.file.ID] = true
		if len(out.images) >= maxPromptImages || reads >= maxImageReads {
			out.warnings = append(out.warnings, "Additional attachments omitted: a turn reads at most 8 files and carries at most 4 images.")
			break
		}
		reads++
		var img protocol.ImageContent
		var err error
		if !canRead {
			err = errors.New("this platform cannot read attachments")
		} else {
			readCtx, readCancel := context.WithTimeout(ctx, imageTimeout)
			img, err = reader.ReadImage(readCtx, f.file)
			readCancel()
		}
		if err == nil && (img.Data == "" || img.Type != "image") {
			err = errors.New("attachment returned no image content")
		}
		if err == nil && len(img.Data) > budget {
			err = errors.New("image exceeds the turn's 10 MiB total image limit")
		}
		note := ""
		if err != nil {
			note = fmt.Sprintf("[attachment %s unavailable: %s]", attachmentName(f.file), err)
			out.warnings = append(out.warnings, note)
		} else {
			budget -= len(img.Data)
			out.images = append(out.images, img)
			note = fmt.Sprintf("[image %d: %s, from %s]", len(out.images), attachmentName(f.file), f.source)
			if f.source == "request" {
				triggerImages++
			}
		}
		if f.source == "request" {
			requestNotes = append(requestNotes, note)
		} else {
			contextNotes = append(contextNotes, note)
		}
	}

	trigger := trimPrompt(m.Text)
	if strings.TrimSpace(trigger) == "" {
		if triggerImages == 0 {
			return out, fmt.Errorf("I could not read the attached images: %s", strings.Join(out.warnings, "\n"))
		}
		trigger = "Please inspect the attached images."
	}
	if len(contextNotes) > 0 {
		// Keep metadata about overheard images inside the conversation block.
		obs.block = strings.TrimSuffix(obs.block, transcriptClose) + strings.Join(contextNotes, "\n") + "\n" + transcriptClose
	}
	if len(requestNotes) > 0 {
		trigger = strings.Join(requestNotes, "\n") + "\n\n" + trigger
	}
	out.text = obs.prompt(trigger)
	return out, nil
}
