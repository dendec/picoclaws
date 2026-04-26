package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// EquipAgent adds shared tools (web, send_file, message, etc.) to an agent instance.
// This is a local implementation moved from the picoclaw library to allow customization.
func (a *WorkerApp) EquipAgent(inst *agent.AgentInstance) {
	cfg := a.Agent.GetConfig()
	msgBus := a.Bus
	allowReadPaths := buildAllowReadPatterns(cfg)

	// 1. Web Tools
	if cfg.Tools.IsToolEnabled("web") {
		searchTool, err := tools.NewWebSearchTool(tools.WebSearchToolOptions{
			BraveAPIKeys:          cfg.Tools.Web.Brave.APIKeys.Values(),
			BraveMaxResults:       cfg.Tools.Web.Brave.MaxResults,
			BraveEnabled:          cfg.Tools.Web.Brave.Enabled,
			TavilyAPIKeys:         cfg.Tools.Web.Tavily.APIKeys.Values(),
			TavilyBaseURL:         cfg.Tools.Web.Tavily.BaseURL,
			TavilyMaxResults:      cfg.Tools.Web.Tavily.MaxResults,
			TavilyEnabled:         cfg.Tools.Web.Tavily.Enabled,
			DuckDuckGoMaxResults:  cfg.Tools.Web.DuckDuckGo.MaxResults,
			DuckDuckGoEnabled:     cfg.Tools.Web.DuckDuckGo.Enabled,
			PerplexityAPIKeys:     cfg.Tools.Web.Perplexity.APIKeys.Values(),
			PerplexityMaxResults:  cfg.Tools.Web.Perplexity.MaxResults,
			PerplexityEnabled:     cfg.Tools.Web.Perplexity.Enabled,
			SearXNGBaseURL:        cfg.Tools.Web.SearXNG.BaseURL,
			SearXNGMaxResults:     cfg.Tools.Web.SearXNG.MaxResults,
			SearXNGEnabled:        cfg.Tools.Web.SearXNG.Enabled,
			GLMSearchAPIKey:       cfg.Tools.Web.GLMSearch.APIKey.String(),
			GLMSearchBaseURL:      cfg.Tools.Web.GLMSearch.BaseURL,
			GLMSearchEngine:       cfg.Tools.Web.GLMSearch.SearchEngine,
			GLMSearchMaxResults:   cfg.Tools.Web.GLMSearch.MaxResults,
			GLMSearchEnabled:      cfg.Tools.Web.GLMSearch.Enabled,
			BaiduSearchAPIKey:     cfg.Tools.Web.BaiduSearch.APIKey.String(),
			BaiduSearchBaseURL:    cfg.Tools.Web.BaiduSearch.BaseURL,
			BaiduSearchMaxResults: cfg.Tools.Web.BaiduSearch.MaxResults,
			BaiduSearchEnabled:    cfg.Tools.Web.BaiduSearch.Enabled,
			Proxy:                 cfg.Tools.Web.Proxy,
		})
		if err == nil && searchTool != nil {
			inst.Tools.Register(searchTool)
		}
	}

	if cfg.Tools.IsToolEnabled("web_fetch") {
		fetchTool, err := tools.NewWebFetchToolWithProxy(
			50000,
			cfg.Tools.Web.Proxy,
			cfg.Tools.Web.Format,
			cfg.Tools.Web.FetchLimitBytes,
			cfg.Tools.Web.PrivateHostWhitelist,
		)
		if err == nil {
			inst.Tools.Register(fetchTool)
		}
	}

	// 2. Messaging Tools
	if cfg.Tools.IsToolEnabled("message") {
		messageTool := tools.NewMessageTool()
		messageTool.SetSendCallback(func(ctx context.Context, channel, chatID, content, replyToMessageID string) error {
			outboundCtx := bus.NewOutboundContext(channel, chatID, replyToMessageID)
			agentID, sessionKey, scope := outboundTurnMetadata(
				tools.ToolAgentID(ctx),
				tools.ToolSessionKey(ctx),
				tools.ToolSessionScope(ctx),
			)
			return msgBus.PublishOutbound(context.Background(), bus.OutboundMessage{
				Context:          outboundCtx,
				AgentID:          agentID,
				SessionKey:       sessionKey,
				Scope:            scope,
				Content:          content,
				ReplyToMessageID: replyToMessageID,
			})
		})
		inst.Tools.Register(messageTool)
	}

	// 3. Media Tools
	if cfg.Tools.IsToolEnabled("send_file") {
		t := tools.NewSendFileTool(
			inst.Workspace,
			cfg.Agents.Defaults.RestrictToWorkspace,
			cfg.Agents.Defaults.GetMaxMediaSize(),
			nil,
			allowReadPaths,
		)
		if a.MediaStore != nil {
			if sf, ok := any(t).(interface{ SetMediaStore(media.MediaStore) }); ok {
				sf.SetMediaStore(a.MediaStore)
			}
		}
		inst.Tools.Register(t)
	}

	if cfg.Tools.IsToolEnabled("load_image") {
		t := tools.NewLoadImageTool(
			inst.Workspace,
			cfg.Agents.Defaults.RestrictToWorkspace,
			cfg.Agents.Defaults.GetMaxMediaSize(),
			nil,
			allowReadPaths,
		)
		if a.MediaStore != nil {
			if sf, ok := any(t).(interface{ SetMediaStore(media.MediaStore) }); ok {
				sf.SetMediaStore(a.MediaStore)
			}
		}
		inst.Tools.Register(t)
	}

	// 4. Subagent Tools
	spawnEnabled := cfg.Tools.IsToolEnabled("spawn")
	spawnStatusEnabled := cfg.Tools.IsToolEnabled("spawn_status")
	if (spawnEnabled || spawnStatusEnabled) && cfg.Tools.IsToolEnabled("subagent") {
		subagentManager := tools.NewSubagentManager(inst.Provider, inst.Model, inst.Workspace)
		subagentManager.SetLLMOptions(inst.MaxTokens, inst.Temperature)

		// Clone the parent's tool registry so subagents can use all
		// tools registered so far but NOT spawn/spawn_status yet.
		subagentManager.SetTools(inst.Tools.Clone())

		if spawnEnabled {
			spawnTool := tools.NewSpawnTool(subagentManager)
			spawnTool.SetSpawner(agent.NewSubTurnSpawner(a.Agent))

			currentAgentID := inst.ID
			spawnTool.SetAllowlistChecker(func(targetAgentID string) bool {
				return a.Agent.GetRegistry().CanSpawnSubagent(currentAgentID, targetAgentID)
			})

			inst.Tools.Register(spawnTool)

			// Also register the synchronous subagent tool
			subagentTool := tools.NewSubagentTool(subagentManager)
			subagentTool.SetSpawner(agent.NewSubTurnSpawner(a.Agent))
			inst.Tools.Register(subagentTool)
		}
		if spawnStatusEnabled {
			inst.Tools.Register(tools.NewSpawnStatusTool(subagentManager))
		}
	}
}

// Helpers replicated from picoclaw/pkg/agent

func buildAllowReadPatterns(cfg *config.Config) []*regexp.Regexp {
	var configured []string
	if cfg != nil {
		configured = cfg.Tools.AllowReadPaths
	}

	compiled, err := compilePatterns(configured)
	if err != nil {
		// Log error but continue with what we have
		logger.ErrorCF("worker", "Failed to compile some allow_read_paths", map[string]any{
			"error": err.Error(),
		})
	}

	mediaDirPattern := regexp.MustCompile(mediaTempDirPattern())
	for _, pattern := range compiled {
		if pattern.String() == mediaDirPattern.String() {
			return compiled
		}
	}

	return append(compiled, mediaDirPattern)
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	var errs []string
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%q: %v", p, err))
			continue
		}
		compiled = append(compiled, re)
	}
	if len(errs) > 0 {
		return compiled, fmt.Errorf("regex compilation errors: %s", strings.Join(errs, "; "))
	}
	return compiled, nil
}

func mediaTempDirPattern() string {
	sep := regexp.QuoteMeta(string(os.PathSeparator))
	return "^" + regexp.QuoteMeta(filepath.Clean(media.TempDir())) + "(?:" + sep + "|$)"
}

func outboundTurnMetadata(
	agentID, sessionKey string,
	scope *session.SessionScope,
) (string, string, *bus.OutboundScope) {
	return agentID, sessionKey, outboundScopeFromSessionScope(scope)
}

func outboundScopeFromSessionScope(scope *session.SessionScope) *bus.OutboundScope {
	if scope == nil {
		return nil
	}
	outboundScope := &bus.OutboundScope{
		Version: scope.Version,
		AgentID: scope.AgentID,
		Channel: scope.Channel,
		Account: scope.Account,
	}
	if len(scope.Dimensions) > 0 {
		outboundScope.Dimensions = append([]string(nil), scope.Dimensions...)
	}
	if len(scope.Values) > 0 {
		outboundScope.Values = make(map[string]string, len(scope.Values))
		for key, value := range scope.Values {
			outboundScope.Values[key] = value
		}
	}
	return outboundScope
}
