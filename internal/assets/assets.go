package assets

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:skeleton
var privateWorkspace embed.FS

//go:embed all:skeleton_default
var publicWorkspace embed.FS

// RestoreWorkspace extracts the embedded workspace, prioritizing "skeleton" if present.
func RestoreWorkspace(destDir string) error {
	sourceFS := privateWorkspace
	sourcePrefix := "skeleton"

	isPrivateEmpty := true
	_ = fs.WalkDir(privateWorkspace, "skeleton", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".md" {
			isPrivateEmpty = false
			return filepath.SkipDir
		}
		return nil
	})

	if isPrivateEmpty {
		sourceFS = publicWorkspace
		sourcePrefix = "skeleton_default"
	}

	return fs.WalkDir(sourceFS, sourcePrefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(sourcePrefix, path)
		if relPath == "." {
			return nil
		}

		targetPath := filepath.Join(destDir, relPath)
		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		data, err := sourceFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		_ = os.MkdirAll(filepath.Dir(targetPath), 0o755)
		return os.WriteFile(targetPath, data, 0o644)
	})
}

// RestoreSkills extracts only the skills directory from the embedded workspace.
func RestoreSkills(destDir string) error {
	sourceFS := privateWorkspace
	sourcePrefix := "skeleton"

	isPrivateEmpty := true
	_ = fs.WalkDir(privateWorkspace, "skeleton", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".md" {
			isPrivateEmpty = false
			return filepath.SkipDir
		}
		return nil
	})

	if isPrivateEmpty {
		sourceFS = publicWorkspace
		sourcePrefix = "skeleton_default"
	}

	skillsPrefix := filepath.Join(sourcePrefix, "skills")

	return fs.WalkDir(sourceFS, skillsPrefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(sourcePrefix, path)
		targetPath := filepath.Join(destDir, relPath)

		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		data, err := sourceFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		_ = os.MkdirAll(filepath.Dir(targetPath), 0o755)
		return os.WriteFile(targetPath, data, 0o644)
	})
}

func RestoreWorkspaceToReader(path string) (io.Reader, error) {
	if r, err := privateWorkspace.Open(filepath.Join("skeleton", path)); err == nil {
		return r, nil
	}
	return publicWorkspace.Open(filepath.Join("skeleton_default", path))
}
