package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/shlex"

	"github.com/rs/zerolog/log"
	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// SkillTool is a dynamic tool that executes a script in a skill directory.
type SkillTool struct {
	name           string
	description    string
	skillDir       string
	engineScript   string
	workspace      string
	chatID         string
	executorURL    string
	autoBackground []string
}

func (t *SkillTool) Name() string {
	return t.name
}

func (t *SkillTool) Description() string {
	relPath := filepath.Join("skills", filepath.Base(t.skillDir), "SKILL.md")
	if t.engineScript != "" {
		return fmt.Sprintf("%s\n\nActive skill. Detailed instructions: %s. Read it before use.", t.description, relPath)
	}
	return fmt.Sprintf("%s\n\nInformational skill. Detailed instructions: %s. Read it to perform tasks manually.", t.description, relPath)
}

func (t *SkillTool) Parameters() map[string]any {
	if t.engineScript == "" {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The subcommand and its arguments to run (e.g., 'submit --prompt \"cat\"'). See instructions below.",
			},
			"background": map[string]any{
				"type":        "boolean",
				"description": "If true, the task will run in the background. DEFAULT IS TRUE FOR 'submit' COMMANDS. Use this for long-running tasks like image generation. I will notify you when it's finished.",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": "Optional: Override the target chat ID for notifications. Use this if you are performing a task for a specific user while in a different context (like heartbeat).",
			},
		},
		"required": []string{"command"},
	}
}

func (t *SkillTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	if t.engineScript == "" {
		// Informational skill: just return the full documentation
		return &tools.ToolResult{
			ForLLM: t.description,
		}
	}

	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return &tools.ToolResult{ForLLM: "missing 'command' argument for active skill", IsError: true}
	}

	background, _ := args["background"].(bool)
	// Safety net: auto-force background for specific subcommands if executor is available
	if t.executorURL != "" && !background {
		fields := strings.Fields(cmdStr)
		if len(fields) > 0 {
			subCmd := fields[0]
			for _, auto := range t.autoBackground {
				if subCmd == auto {
					background = true
					break
				}
			}
		}
	}

	if background && t.executorURL != "" {
		chatID, _ := args["chat_id"].(string)
		return t.executeBackground(ctx, cmdStr, chatID)
	}

	// Split the command string into parts, respecting quotes using shlex
	parts, err := shlex.Split(cmdStr)
	if err != nil {
		return &tools.ToolResult{ForLLM: "failed to parse command: " + err.Error(), IsError: true}
	}
	if len(parts) == 0 {
		return &tools.ToolResult{ForLLM: "empty command", IsError: true}
	}

	// Build the full command: python3 skills/<name>/<engine> <parts...>
	fullArgs := append([]string{t.engineScript}, parts...)
	cmd := exec.CommandContext(ctx, "python3", fullArgs...)
	cmd.Dir = t.workspace

	// Ensure we pass the same environment (PATH, etc.)
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := fmt.Sprintf("command failed: %v\nOutput: %s", err, string(output))
		// Add a helpful hint for argparse errors (exit status 2 usually means usage error)
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
			msg += "\n\nNote: This looks like an argument error. If you are passing values with spaces (like model names), make sure to wrap them in double quotes, e.g., --name \"My Model Name\"."
		}
		return &tools.ToolResult{
			ForLLM:  msg,
			IsError: true,
			Err:     err,
		}
	}

	return &tools.ToolResult{
		ForLLM: string(output),
	}
}

func (t *SkillTool) executeBackground(ctx context.Context, command string, chatIDOverride string) *tools.ToolResult {
	targetChatID := t.chatID
	if chatIDOverride != "" {
		targetChatID = chatIDOverride
	}

	payload := map[string]any{
		"skill":   t.name,
		"engine":  t.engineScript,
		"command": command,
		"chat_id": targetChatID,
	}
	data, _ := json.Marshal(payload)

	// Fire and forget (almost)
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, "POST", t.executorURL, bytes.NewBuffer(data))
	if err != nil {
		return &tools.ToolResult{ForLLM: "failed to create background request: " + err.Error(), IsError: true}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return &tools.ToolResult{
			ForLLM: "failed to trigger background task: " + err.Error(), IsError: true,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		result, _ := io.ReadAll(resp.Body)
		return &tools.ToolResult{
			ForLLM: fmt.Sprintf("executor returned error status: %d: %s", resp.StatusCode, string(result)), IsError: true,
		}
	}

	result, _ := io.ReadAll(resp.Body)
	return &tools.ToolResult{
		ForLLM: fmt.Sprintf("Task submitted to background. Executor response: %s\n[SYSTEM: Task is now running in the background. I will notify you in this chat when it's done. FINISH YOUR TURN NOW and do not call 'status' or 'check' immediately.]", string(result)),
	}
}


// RegisterSkills scans the workspace for skills and registers them as tools.
func (a *WorkerApp) RegisterSkills(inst *agent.AgentInstance, chatID string) {
	skillsDir := filepath.Join(inst.Workspace, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error().Err(err).Msg("Failed to read skills directory")
		}
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillName := entry.Name()
		skillPath := filepath.Join(skillsDir, skillName)
		skillMdPath := filepath.Join(skillPath, "SKILL.md")

		if _, err := os.Stat(skillMdPath); err != nil {
			continue
		}

		// Parse name and description from SKILL.md
		name, desc, err := parseSkillMetadata(skillMdPath)
		if err != nil {
			log.Warn().Err(err).Str("skill", skillName).Msg("Failed to parse SKILL.md")
			name = skillName
			desc = "Skill: " + skillName
		}

		// Find the engine script (usually <name>_engine.py)
		engineScript := ""
		files, _ := os.ReadDir(skillPath)
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".py") {
				// Prefer *_engine.py or engine.py, but accept any .py as fallback
				engineScript = filepath.Join("skills", skillName, f.Name())
				if strings.HasSuffix(f.Name(), "_engine.py") || f.Name() == "engine.py" {
					break
				}
			}
		}

		tool := &SkillTool{
			name:         name,
			description:  desc,
			skillDir:     skillPath,
			engineScript: engineScript,
			workspace:    inst.Workspace,
			chatID:       chatID,
			executorURL:  a.TaskExecutorURL,
		}

		// Configure auto-background for specific skills
		if name == "draw" {
			tool.autoBackground = []string{"submit"}
		}

		inst.Tools.Register(tool)

		if engineScript != "" {
			log.Debug().Str("skill", name).Msg("Registered active skill tool")
		} else {
			log.Debug().Str("skill", name).Msg("Registered informational skill tool")
		}
	}
}

func parseSkillMetadata(path string) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}

	content := string(data)
	lines := strings.Split(content, "\n")
	var name, desc string
	for _, line := range lines {
		if strings.HasPrefix(line, "---") {
			continue
		}
		if strings.HasPrefix(line, "name:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		}
		if strings.HasPrefix(line, "description:") {
			desc = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
		}
	}

	if name == "" {
		return "", "", fmt.Errorf("missing name in SKILL.md")
	}

	return name, desc, nil
}
