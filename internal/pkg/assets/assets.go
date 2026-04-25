package assets

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// DefaultWorkspace holds the initial workspace skeleton ( IDENTITY.md, SOUL.md, skills/, etc. )
//
//go:embed all:skeleton
var DefaultWorkspace embed.FS

// RestoreWorkspace extracts the embedded workspace files into a destination directory.
func RestoreWorkspace(destDir string) error {
	return fs.WalkDir(DefaultWorkspace, "skeleton", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Create the relative path from the root of "skeleton"
		relPath, err := filepath.Rel("skeleton", path)
		if err != nil {
			return err
		}

		// Skip the root "skeleton" directory itself
		if relPath == "." {
			return nil
		}

		targetPath := filepath.Join(destDir, relPath)

		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		// Regular file
		data, err := DefaultWorkspace.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read embedded file %s: %w", path, err)
		}

		// Ensure parent directory exists (just in case)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}

		return os.WriteFile(targetPath, data, 0o644)
	})
}

// RestoreWorkspaceToReader extracts the embedded workspace as a reader (optional, if needed)
func RestoreWorkspaceToReader(path string) (io.Reader, error) {
	return DefaultWorkspace.Open(filepath.Join("skeleton", path))
}
