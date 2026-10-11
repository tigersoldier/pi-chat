package chattools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// validatePrivateDirectory requires an owner-only leaf and ancestors that
// other users cannot replace. Sticky shared directories such as /tmp are safe
// ancestors because their entries cannot be renamed by non-owners.
func validatePrivateDirectory(path string) error {
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("chat tools: inspect private directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("chat tools: private path must be a real directory")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("chat tools: private directory %s must have mode 0700, got %04o", path, info.Mode().Perm())
	}

	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return fmt.Errorf("chat tools: inspect private directory ancestor: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("chat tools: private directory ancestors must be real directories")
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("chat tools: directory ancestor %s is writable by other users without the sticky bit", parent)
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	return nil
}
