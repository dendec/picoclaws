package archive

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Archive creates a tar.zst archive from a directory and returns the bytes.
func Archive(srcDir string) ([]byte, error) {
	var buf bytes.Buffer

	// Create zstd writer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd writer: %w", err)
	}

	tw := tar.NewWriter(zw)

	// Walk through the directory and add files to the tar archive
	err = filepath.Walk(srcDir, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Create a local name for the file in the archive
		relPath, err := filepath.Rel(srcDir, file)
		if err != nil {
			return err
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		// Create a tar header
		header, err := tar.FileInfoHeader(fi, relPath)
		if err != nil {
			return err
		}
		header.Name = relPath

		// Write the header
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		// Handle different file types
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(file)
			if err != nil {
				return fmt.Errorf("failed to read link %s: %w", file, err)
			}
			header.Linkname = target
			header.Typeflag = tar.TypeSymlink
		} else if fi.Mode().IsRegular() {
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()

			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to walk directory: %w", err)
	}

	// Close writers in order
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close zstd writer: %w", err)
	}

	return buf.Bytes(), nil
}

// Unarchive extracts a tar.zst archive bytes to a destination directory.
func Unarchive(data []byte, destDir string) error {
	// Create zstd reader
	zr, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create zstd reader: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)

	// Ensure destination directory exists
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar header: %w", err)
		}

		// Clean the path to prevent directory traversal
		target := filepath.Join(destDir, filepath.Clean(header.Name))

		// Check for directory traversal (though Clean should handle it)
		if !strings.HasPrefix(target, filepath.Clean(destDir)) {
			return fmt.Errorf("illegal file path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("failed to create directory: %w", err)
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("failed to create parent directory: %w", err)
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("failed to create file: %w", err)
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("failed to copy file content: %w", err)
			}
			f.Close()

		case tar.TypeSymlink:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("failed to create parent directory: %w", err)
			}
			// Remove existing file/link if any
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("failed to create symlink: %w", err)
			}
		}
	}

	return nil
}
