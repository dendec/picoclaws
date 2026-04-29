package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// SkillTool is a dynamic tool that executes a script in a skill directory.
type SkillTool struct {
	name         string
	description  string
	skillDir     string
	engineScript string
	workspace    string
}

func (t *SkillTool) Name() string {
	return t.name
}

func (t *SkillTool) Description() string {
	if t.engineScript != "" {
		return t.description + "\n\nActive skill: Use this tool to run subcommands (e.g., submit, check, download). Detailed instructions are included below."
	}
	return t.description + "\n\nInformational skill: Call this tool to see detailed instructions on how to perform these tasks manually using 'exec' or other tools."
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

	// Split the command string into parts, respecting quotes
	parts := splitArgs(cmdStr)
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

// splitArgs performs simple shell-style argument splitting, respecting double quotes.
func splitArgs(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			inQuotes = !inQuotes
			continue
		}
		if c == ' ' && !inQuotes {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteByte(c)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// RegisterSkills scans the workspace for skills and registers them as tools.
func (a *WorkerApp) RegisterSkills(inst *agent.AgentInstance) {
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
			if strings.HasSuffix(f.Name(), "_engine.py") {
				engineScript = filepath.Join("skills", skillName, f.Name())
				break
			}
		}

		inst.Tools.Register(&SkillTool{
			name:         name,
			description:  desc,
			skillDir:     skillPath,
			engineScript: engineScript,
			workspace:    inst.Workspace,
		})
		
		if engineScript != "" {
			log.Info().Str("skill", name).Msg("Registered active skill tool")
		} else {
			log.Info().Str("skill", name).Msg("Registered informational skill tool")
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
	headerEnd := 0
	for i, line := range lines {
		if strings.HasPrefix(line, "---") {
			if i == 0 {
				continue
			}
			headerEnd = i + 1
			break
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

	// The rest of the file (after ---) contains detailed instructions
	instructions := ""
	if headerEnd > 0 && headerEnd < len(lines) {
		instructions = strings.Join(lines[headerEnd:], "\n")
	}

	fullDesc := desc
	if instructions != "" {
		fullDesc = fmt.Sprintf("%s\n\n## Instructions:\n%s", desc, instructions)
	}

	return name, fullDesc, nil
}
