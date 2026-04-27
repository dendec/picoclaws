package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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
