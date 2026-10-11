package chattools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallExtensionPinsSocketWithoutCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions", "chat-tools.ts")
	socket := filepath.Join(t.TempDir(), "runtime", "tools.sock")
	if err := InstallExtension(path, socket); err != nil {
		t.Fatalf("InstallExtension: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `const SOCKET_PATH = "`+socket+`";`) {
		t.Errorf("installed extension does not use the configured socket")
	}
	if strings.Contains(string(got), "__PI_CHAT_") || !strings.Contains(string(got), "const PROTOCOL_VERSION = 1;") ||
		!strings.Contains(string(got), `const EXTENSION_VERSION = "`+ExtensionVersion+`";`) {
		t.Errorf("installed extension has unresolved or mismatched protocol markers")
	}
	for _, name := range []string{ToolReadThread, ToolReadMessage, ToolGetUser, HealthCommand} {
		if !strings.Contains(string(got), name) {
			t.Errorf("installed extension is missing %q", name)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("extension mode = %o, want 600", info.Mode().Perm())
	}
}

func TestAppendExtensionIsIdempotent(t *testing.T) {
	path := "/opt/pi-chat/chat-tools.ts"
	for _, args := range [][]string{
		{"--provider", "openai"},
		{"--extension", path},
		{"-e", path},
		{"--extension=" + path},
	} {
		got := AppendExtension(args, path)
		count := 0
		for i, arg := range got {
			if arg == "--extension" && i+1 < len(got) && got[i+1] == path {
				count++
			}
			if arg == "-e" && i+1 < len(got) && got[i+1] == path {
				count++
			}
			if arg == "--extension="+path {
				count++
			}
		}
		if count != 1 {
			t.Errorf("AppendExtension(%v) = %v, counted %d extensions", args, got, count)
		}
	}
}

func TestInstallExtensionRefusesNonRegularPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat-tools.ts")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallExtension(path, filepath.Join(t.TempDir(), "tools.sock")); err == nil {
		t.Fatal("InstallExtension replaced a directory")
	}
}
