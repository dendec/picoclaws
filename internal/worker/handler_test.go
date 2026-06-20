package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/media"
)

type mockMediaStore struct {
	media.MediaStore
	resolveFn func(ref string) (string, media.MediaMeta, error)
}

func (m *mockMediaStore) ResolveWithMeta(ref string) (string, media.MediaMeta, error) {
	if m.resolveFn != nil {
		return m.resolveFn(ref)
	}
	return "", media.MediaMeta{}, nil
}

func TestWorkspaceLifecycle(t *testing.T) {
	ctx := context.Background()
	
	// Create a temp base directory for the worker
	tmpBase, err := os.MkdirTemp("", "picoclaws-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpBase)

	app := &WorkerApp{
		BaseDir: tmpBase,
	}

	chatID := "test-user-123"
	chatWorkspace := app.getWorkspacePath(chatID)

	t.Run("InitialPrepare", func(t *testing.T) {
		// Should create directories and restore assets
		isNew, err := app.prepareWorkspace(ctx, nil, chatID, chatWorkspace)
		if err != nil {
			t.Errorf("prepareWorkspace failed: %v", err)
		}
		if !isNew {
			t.Error("expected isNew to be true for first-time prepare")
		}

		// Check if directory exists
		if _, err := os.Stat(chatWorkspace); err != nil {
			t.Errorf("chat workspace not created: %v", err)
		}
		
		// Check if a known asset is restored (assuming MEMORY.md is in skeleton)
		memoryFile := filepath.Join(chatWorkspace, "memory", "MEMORY.md")
		if _, err := os.Stat(memoryFile); err != nil {
			t.Errorf("assets not restored to %s: %v", memoryFile, err)
		}
	})

	t.Run("ResetWorkspace", func(t *testing.T) {
		// Add a dummy file to workspace
		dummyFile := filepath.Join(chatWorkspace, "dummy.txt")
		_ = os.WriteFile(dummyFile, []byte("data"), 0644)

		// Reset
		err := app.resetWorkspace(ctx, chatID)
		if err != nil {
			t.Errorf("resetWorkspace failed: %v", err)
		}

		// Verify deletion
		if _, err := os.Stat(chatWorkspace); err == nil {
			t.Error("chat workspace should have been deleted")
		}
	})

	t.Run("PrepareAfterReset", func(t *testing.T) {
		// Should restore everything again
		isNew, err := app.prepareWorkspace(ctx, nil, chatID, chatWorkspace)
		if err != nil {
			t.Errorf("prepareWorkspace failed after reset: %v", err)
		}
		if !isNew {
			t.Error("expected isNew to be true after reset")
		}
		
		if _, err := os.Stat(chatWorkspace); err != nil {
			t.Error("chat workspace should be recreated")
		}
	})
}

func TestMaterializeMediaHotSwap(t *testing.T) {
	ctx := context.Background()
	
	tmpDir, err := os.MkdirTemp("", "picoclaws-hotswap-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a dummy source file
	srcFile := filepath.Join(tmpDir, "source_soul.md")
	content := []byte("new soul content")
	if err := os.WriteFile(srcFile, content, 0644); err != nil {
		t.Fatalf("failed to write source file: %v", err)
	}

	// Create a dummy large source file (> 2KB)
	largeSrcFile := filepath.Join(tmpDir, "large_soul.md")
	largeContent := make([]byte, 2049)
	copy(largeContent, []byte("large soul content"))
	if err := os.WriteFile(largeSrcFile, largeContent, 0644); err != nil {
		t.Fatalf("failed to write large source file: %v", err)
	}

	app := &WorkerApp{
		BaseDir: tmpDir,
	}

	chatWorkspace := filepath.Join(tmpDir, "chat-123")
	if err := os.MkdirAll(chatWorkspace, 0755); err != nil {
		t.Fatalf("failed to create chat workspace: %v", err)
	}

	// Setup initial SOUL.md
	soulPath := filepath.Join(chatWorkspace, "SOUL.md")
	initialSoul := []byte("initial soul content")
	if err := os.WriteFile(soulPath, initialSoul, 0644); err != nil {
		t.Fatalf("failed to write initial soul: %v", err)
	}

	mStore := &mockMediaStore{
		resolveFn: func(ref string) (string, media.MediaMeta, error) {
			if ref == "ref-1" {
				return srcFile, media.MediaMeta{Filename: "soul.md"}, nil
			}
			if ref == "ref-large" {
				return largeSrcFile, media.MediaMeta{Filename: "soul.md"}, nil
			}
			return "", media.MediaMeta{}, nil
		},
	}
	app.MediaStore = mStore

	inMsg := &bus.InboundMessage{
		Media: []string{"ref-1"},
	}

	app.materializeMedia(ctx, chatWorkspace, inMsg)

	// Check if SOUL.md is overwritten
	data, err := os.ReadFile(soulPath)
	if err != nil {
		t.Fatalf("failed to read soul path: %v", err)
	}
	if string(data) != "new soul content" {
		t.Errorf("expected new soul content, got %q", string(data))
	}

	// Check if SOUL.bak is created with initial content
	bakPath := filepath.Join(chatWorkspace, "SOUL.bak")
	bakData, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatalf("failed to read backup path: %v", err)
	}
	if string(bakData) != "initial soul content" {
		t.Errorf("expected initial soul content in backup, got %q", string(bakData))
	}

	// Test 2: Uploading a file > 2KB (should be ignored)
	inMsgLarge := &bus.InboundMessage{
		Media: []string{"ref-large"},
	}
	// Reset SOUL.md
	_ = os.WriteFile(soulPath, []byte("before-large"), 0644)
	_ = os.Remove(bakPath)

	app.materializeMedia(ctx, chatWorkspace, inMsgLarge)

	// Check that SOUL.md was NOT overwritten
	dataLarge, err := os.ReadFile(soulPath)
	if err != nil {
		t.Fatalf("failed to read soul path: %v", err)
	}
	if string(dataLarge) != "before-large" {
		t.Errorf("expected SOUL.md to remain unchanged, got %q", string(dataLarge))
	}
}
