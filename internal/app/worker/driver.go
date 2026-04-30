package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"picoclaws/internal/platform/telegram/tgutil"

	"github.com/rs/zerolog/log"
	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type processOptions struct {
	SessionKey           string
	Channel              string
	ChatID               string
	MessageID            string
	UserMessage          string
	Media                []string
	DefaultResponse      string
	EnableSummary        bool
	SendResponse         bool
	SenderID             string
	SenderDisplayName    string
	ReasoningChannelID   string
	SuppressToolFeedback bool
	SuppressReasoning    bool
	DisableHistory       bool
}

// Driver provides single-shot agent execution logic.
type Driver struct {
	Bus        *bus.MessageBus
	MediaStore media.MediaStore
	Config     *config.Config
}

const MaxVisibleHistory = 10

func (d *Driver) RunAgent(ctx context.Context, inst *agent.AgentInstance, opts processOptions) (string, error) {
	// 1. Build messages
	history := inst.Sessions.GetHistory(opts.SessionKey)
	// Soft-limit history for the LLM prompt
	// This keeps the prompt small but preserves the full file on disk for tools like grep.
	llmHistory := history
	if len(llmHistory) > MaxVisibleHistory {
		llmHistory = llmHistory[len(llmHistory)-MaxVisibleHistory:]
	}
	summary := inst.Sessions.GetSummary(opts.SessionKey)

	if len(opts.Media) > 0 {
		opts.UserMessage += "\n\n(Use tools for inbox/ files)"
	}

	messages := inst.ContextBuilder.BuildMessages(
		llmHistory,
		summary,
		opts.UserMessage,
		opts.Media,
		opts.Channel,
		opts.ChatID,
		opts.SenderID,
		opts.SenderDisplayName,
	)

	// Resolve media:// refs
	maxMediaSize := d.Config.Agents.Defaults.GetMaxMediaSize()
	messages = d.resolveMediaRefs(messages, int64(maxMediaSize))

	// 2. Log and save user message
	log.Ctx(ctx).Info().
		Str("chat_id", opts.ChatID).
		Str("sender_id", opts.SenderID).
		Str("sender_name", opts.SenderDisplayName).
		Str("message", opts.UserMessage).
		Int("media_count", len(opts.Media)).
		Msg("User message received")
	if !opts.DisableHistory {
		inst.Sessions.AddMessage(opts.SessionKey, "user", opts.UserMessage)
	}

	// 3. Run LLM iteration loop
	finalContent, iteration, sentMessages, totalUsage, err := d.runIteration(ctx, inst, messages, opts)
	if err != nil {
		return "", err
	}

	if finalContent == "" {
		finalContent = opts.DefaultResponse
	}

	// 4. Save final assistant message to session
	if !opts.DisableHistory {
		inst.Sessions.AddMessage(opts.SessionKey, "assistant", finalContent)
		inst.Sessions.Save(opts.SessionKey)
	}

	// 5. Send response via bus if requested
	// Suppress if message was already sent via tool AND final content is redundant
	suppress := false
	for _, m := range sentMessages {
		if isRedundant(m, finalContent) {
			suppress = true
			break
		}
	}
	if !suppress && len(sentMessages) > 0 && (finalContent == "" || finalContent == opts.DefaultResponse) {
		suppress = true
	}

	// Suppress leaked raw tool calls (Gemma/special token artifacts)
	if strings.Contains(finalContent, "call:exec{") ||
		strings.Contains(finalContent, "call:message{") ||
		strings.Contains(finalContent, "call:reaction{") ||
		strings.Contains(finalContent, "<tool_call|>") ||
		strings.Contains(finalContent, "<|\"|>") {
		suppress = true
	}

	if opts.SendResponse && !suppress {
		// Clean up reasoning tags if they leaked into the final response
		cleanContent := finalContent
		if strings.Contains(cleanContent, "<thought") {
			re := regexp.MustCompile(`(?s)<thought.*?>.*?</thought>|<thought.*?>`)
			cleanContent = re.ReplaceAllString(cleanContent, "")
			cleanContent = strings.TrimSpace(cleanContent)
		}

		if cleanContent != "" {
			_ = d.Bus.PublishOutbound(ctx, bus.OutboundMessage{
				Channel: opts.Channel,
				ChatID:  opts.ChatID,
				Content: cleanContent,
			})
		}
	}

	log.Ctx(ctx).Info().
		Str("agent_id", inst.ID).
		Str("chat_id", opts.ChatID).
		Str("session_key", opts.SessionKey).
		Int("iterations", iteration).
		Interface("usage", totalUsage).
		Str("response", finalContent).
		Msg("Agent processing complete")

	return finalContent, nil
}

func (d *Driver) runIteration(ctx context.Context, inst *agent.AgentInstance, messages []providers.Message, opts processOptions) (string, int, []string, map[string]providers.UsageInfo, error) {
	iteration := 0
	var finalContent string
	var sentMessages []string
	totalUsage := make(map[string]providers.UsageInfo)

	// Simple active model selection (ignore routing for now, or implement if needed)
	activeModel := inst.Model

	for {
		iteration++
		if iteration > inst.MaxIterations {
			log.Ctx(ctx).Warn().Int("iterations", iteration).Msg("Max iterations reached")
			break
		}

		// Check for soft timeout (leave 10s for the final LLM summary or graceful exit)
		if deadline, ok := ctx.Deadline(); ok {
			if time.Until(deadline) < 10*time.Second {
				log.Ctx(ctx).Warn().Msg("Soft timeout reached, stopping iterations")
				break
			}
		}

		providerToolDefs := inst.Tools.ToProviderDefs()

		llmOpts := map[string]any{
			"max_tokens":       inst.MaxTokens,
			"temperature":      inst.Temperature,
			"prompt_cache_key": inst.ID,
		}

		// Inject dynamic time hint to help agent plan its iterations
		iterMessages := messages
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline).Round(time.Second)
			hint := fmt.Sprintf("System: Remaining time: %s.", remaining)
			if remaining < 20*time.Second {
				hint += " Time is CRITICAL. Wrap up and provide the final answer NOW."
			} else {
				hint += " Plan your tool calls accordingly."
			}

			iterMessages = append([]providers.Message{}, messages...)
			iterMessages = append(iterMessages, providers.Message{
				Role:    "system",
				Content: hint,
			})
		}

		log.Ctx(ctx).Debug().
			Interface("messages", iterMessages).
			Interface("tools", providerToolDefs).
			Str("activeModel", activeModel).
			Interface("llmOpts", llmOpts).
			Msg("Sending request to LLM")

		response, err := inst.Provider.Chat(ctx, iterMessages, providerToolDefs, activeModel, llmOpts)
		if err != nil {
			return "", iteration, sentMessages, totalUsage, fmt.Errorf("LLM call failed: %w", err)
		}

		// Accumulate and log usage for this iteration
		if response.Usage != nil {
			u := totalUsage[activeModel]
			u.PromptTokens += response.Usage.PromptTokens
			u.CompletionTokens += response.Usage.CompletionTokens
			u.TotalTokens += response.Usage.TotalTokens
			totalUsage[activeModel] = u

			log.Ctx(ctx).Info().
				Str("chat_id", opts.ChatID).
				Str("model", activeModel).
				Int("iteration", iteration).
				Int("prompt_tokens", response.Usage.PromptTokens).
				Int("completion_tokens", response.Usage.CompletionTokens).
				Int("total_tokens", response.Usage.TotalTokens).
				Msg("Iteration usage")
		}

		// Handle reasoning (async best-effort)
		if response.Reasoning != "" {
			// Always log reasoning to system log
			log.Ctx(ctx).Info().
				Str("chat_id", opts.ChatID).
				Str("reasoning", response.Reasoning).
				Msg("Agent reasoning")

			if !opts.SuppressReasoning {
				go func() {
					pubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = d.Bus.PublishOutbound(pubCtx, bus.OutboundMessage{
						Channel: opts.Channel,
						ChatID:  opts.ChatID,
						Content: response.Reasoning,
					})
				}()
			}
		}

		if len(response.ToolCalls) == 0 {
			finalContent = response.Content
			if finalContent == "" && response.ReasoningContent != "" {
				finalContent = response.ReasoningContent
			}
			break
		}

		// Process Tool Calls
		assistantMsg := providers.Message{
			Role:             "assistant",
			Content:          response.Content,
			ReasoningContent: response.ReasoningContent,
		}

		for _, tc := range response.ToolCalls {
			tcNorm := providers.NormalizeToolCall(tc)
			argsJSON, _ := json.Marshal(tcNorm.Arguments)
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, providers.ToolCall{
				ID:   tcNorm.ID,
				Type: "function",
				Name: tcNorm.Name,
				Function: &providers.FunctionCall{
					Name:             tcNorm.Name,
					Arguments:        string(argsJSON),
					ThoughtSignature: tcNorm.ThoughtSignature,
				},
				ExtraContent:     tcNorm.ExtraContent,
				ThoughtSignature: tcNorm.ThoughtSignature,
			})
		}

		// Add to local history for the next generation in THIS turn
		messages = append(messages, assistantMsg)

		// Log tool calls
		for _, tc := range assistantMsg.ToolCalls {
			log.Ctx(ctx).Info().
				Str("chat_id", opts.ChatID).
				Str("tool", tc.Name).
				Str("tool_call_id", tc.ID).
				Str("arguments", tc.Function.Arguments).
				Int("iteration", iteration).
				Msg("Tool call")
		}

		// Execute tools in parallel
		type result struct {
			idx int
			res *tools.ToolResult
			tc  providers.ToolCall
		}
		results := make([]result, len(assistantMsg.ToolCalls))
		var wg sync.WaitGroup
		for i, tc := range assistantMsg.ToolCalls {
			wg.Add(1)
			go func(idx int, t providers.ToolCall) {
				defer wg.Done()
				var args map[string]any
				_ = json.Unmarshal([]byte(t.Function.Arguments), &args)
				// Manually inject message context since picoclaw library doesn't do it in ExecuteWithContext
				toolCtx := tools.WithToolInboundContext(ctx, opts.Channel, opts.ChatID, opts.MessageID, "")
				res := inst.Tools.ExecuteWithContext(toolCtx, t.Name, args, opts.Channel, opts.ChatID, nil)
				results[idx] = result{idx: idx, res: res, tc: t}
			}(i, tc)
		}
		wg.Wait()

		// Save Assistant message to session ONLY after we have results ready to follow it
		if !opts.DisableHistory {
			inst.Sessions.AddFullMessage(opts.SessionKey, assistantMsg)
		}

		// Handle and save results
		for _, r := range results {
			// Always log tool results to system log
			log.Ctx(ctx).Info().
				Str("chat_id", opts.ChatID).
				Str("tool", r.tc.Name).
				Str("tool_call_id", r.tc.ID).
				Str("result", r.res.ForLLM).
				Bool("is_error", r.res.IsError).
				Bool("silent", r.res.Silent).
				Msg("Tool result")

			if r.tc.Name == "message" {
				var args struct {
					Content string `json:"content"`
				}
				_ = json.Unmarshal([]byte(r.tc.Function.Arguments), &args)
				if args.Content != "" {
					sentMessages = append(sentMessages, args.Content)
				}
			}

			// Immediate feedback for user if not silent and not suppressed
			if !opts.SuppressToolFeedback && !r.res.Silent && r.res.ForUser != "" {
				_ = d.Bus.PublishOutbound(ctx, bus.OutboundMessage{
					Channel: opts.Channel,
					ChatID:  opts.ChatID,
					Content: r.res.ForUser,
				})
			}

			// Media results
			if len(r.res.Media) > 0 {
				parts := make([]bus.MediaPart, 0, len(r.res.Media))
				for _, ref := range r.res.Media {
					part := bus.MediaPart{Ref: ref}
					if d.MediaStore != nil {
						if _, meta, err := d.MediaStore.ResolveWithMeta(ref); err == nil {
							part.Filename = meta.Filename
							part.ContentType = meta.ContentType
							part.Type = tgutil.InferMediaType(meta.Filename, meta.ContentType)
						}
					}
					parts = append(parts, part)
				}
				_ = d.Bus.PublishOutboundMedia(ctx, bus.OutboundMediaMessage{
					Channel: opts.Channel,
					ChatID:  opts.ChatID,
					Parts:   parts,
				})
			}

			contentForLLM := r.res.ForLLM
			if contentForLLM == "" && r.res.Err != nil {
				contentForLLM = r.res.Err.Error()
			}
			toolResultMsg := providers.Message{
				Role:       "tool",
				Content:    contentForLLM,
				ToolCallID: r.tc.ID,
			}
			messages = append(messages, toolResultMsg)
			if !opts.DisableHistory {
				inst.Sessions.AddFullMessage(opts.SessionKey, toolResultMsg)
			}
		}

		inst.Tools.TickTTL()
	}

	return finalContent, iteration, sentMessages, totalUsage, nil
}

func (d *Driver) resolveMediaRefs(messages []providers.Message, maxSize int64) []providers.Message {
	if d.MediaStore == nil {
		return messages
	}
	for i := range messages {
		for j := range messages[i].Media {
			ref := messages[i].Media[j]
			if strings.HasPrefix(ref, "media://") {
				path, _, err := d.MediaStore.ResolveWithMeta(ref)
				if err == nil {
					// In a real implementation we might convert to base64 or pass differently.
					// Official picoclaw converts to data URL here.
					if data, err := d.encodeFileToBase64(path, maxSize); err == nil {
						messages[i].Media[j] = data
					}
				}
			}
		}
	}
	return messages
}

func (d *Driver) encodeFileToBase64(path string, maxSize int64) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxSize {
		return "", fmt.Errorf("file too large: %d > %d", info.Size(), maxSize)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	// Simplistic MIME detection for the data URL
	ext := filepath.Ext(path)
	mime := "application/octet-stream"
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".png":
		mime = "image/png"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	case ".ogg":
		mime = "audio/ogg"
	case ".mp3":
		mime = "audio/mpeg"
	case ".mp4":
		mime = "video/mp4"
	}

	encoded := "data:" + mime + ";base64," + strings.TrimSpace(base64.StdEncoding.EncodeToString(data))
	return encoded, nil
}

func isRedundant(original, final string) bool {
	origWords := getWords(original)
	finalWords := getWords(final)
	if len(finalWords) == 0 {
		return true
	}

	matches := 0
	for fw := range finalWords {
		if origWords[fw] {
			matches++
		}
	}

	// If more than 80% of words in the final response are already in the original
	overlap := float64(matches) / float64(len(finalWords))
	return overlap > 0.8
}

func getWords(s string) map[string]bool {
	s = strings.ToLower(s)
	reg := regexp.MustCompile(`[a-z0-9а-яё]+`)
	words := reg.FindAllString(s, -1)

	wordSet := make(map[string]bool)
	for _, w := range words {
		if len(w) > 1 { // Ignore single letters/digits
			wordSet[w] = true
		}
	}
	return wordSet
}
