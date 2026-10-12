package chattools

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// extensionTemplate is installed at one stable absolute path and carries only
// the local broker socket path, never an app credential.
//
//go:embed extension/chat-tools.ts.tmpl
var extensionTemplate string

// InstallExtension atomically installs the trusted Pi extension at path.
func InstallExtension(path, socketPath string) error {
	if !filepath.IsAbs(path) || !filepath.IsAbs(socketPath) {
		return errors.New("chat tools: extension and socket paths must be absolute")
	}
	socketMarker := `"__PI_CHAT_SOCKET_PATH__"`
	protocolMarker := "__PI_CHAT_PROTOCOL_VERSION__"
	versionMarker := `"__PI_CHAT_EXTENSION_VERSION__"`
	if strings.Count(extensionTemplate, socketMarker) != 1 || strings.Count(extensionTemplate, protocolMarker) != 1 ||
		strings.Count(extensionTemplate, versionMarker) != 1 {
		return errors.New("chat tools: extension template is missing or duplicates a configuration marker")
	}
	encodedPath, err := json.Marshal(socketPath)
	if err != nil {
		return fmt.Errorf("chat tools: encode socket path: %w", err)
	}
	encodedVersion, err := json.Marshal(ExtensionVersion)
	if err != nil {
		return fmt.Errorf("chat tools: encode extension version: %w", err)
	}
	content := strings.Replace(extensionTemplate, socketMarker, string(encodedPath), 1)
	content = strings.Replace(content, protocolMarker, strconv.Itoa(ProtocolVersion), 1)
	content = strings.Replace(content, versionMarker, string(encodedVersion), 1)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("chat tools: create extension directory: %w", err)
	}
	if err := validatePrivateDirectory(dir); err != nil {
		return fmt.Errorf("chat tools: extension directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("chat tools: refusing to replace a non-regular extension path")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("chat tools: inspect extension path: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".chat-tools-*.ts")
	if err != nil {
		return fmt.Errorf("chat tools: stage extension: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chat tools: secure staged extension: %w", err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chat tools: write extension: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chat tools: sync extension: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("chat tools: close extension: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("chat tools: install extension: %w", err)
	}
	return nil
}

// AppendExtension adds the installed extension exactly once to an accepted
// Pi argument list.
func AppendExtension(args []string, path string) []string {
	clean := filepath.Clean(path)
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--extension" || args[i] == "-e") && i+1 < len(args):
			if filepath.Clean(args[i+1]) == clean {
				return slices.Clone(args)
			}
			i++
		case strings.HasPrefix(args[i], "--extension="):
			if filepath.Clean(strings.TrimPrefix(args[i], "--extension=")) == clean {
				return slices.Clone(args)
			}
		}
	}
	return append(slices.Clone(args), "--extension", path)
}
