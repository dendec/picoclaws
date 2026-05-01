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
	startTime := time.Now()
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

	// 2. Save user message to history
	if !opts.DisableHistory {
		inst.Sessions.AddMessage(opts.SessionKey, "user", opts.UserMessage)
	}

	// 3. Run LLM iteration loop
	finalContent, iteration, sentMessages, totalUsage, err := d.runIteration(ctx, inst, messages, opts)
	if err != nil {
		return "", err
	}

	// 4. Save final assistant message to session
	if !opts.DisableHistory && finalContent != "" {
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
	if !suppress && len(sentMessages) > 0 && finalContent == "" {
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
		Str("session_key", opts.SessionKey).
		Int("iterations", iteration).
		Interface("usage", totalUsage).
		Str("response", finalContent).
		Int64("duration_ms", time.Since(startTime).Milliseconds()).
		Msg("Agent processing complete")

	return finalContent, nil
}

func (d *Driver) runIteration(ctx context.Context, inst *agent.AgentInstance, messages []providers.Message, opts processOptions) (string, int, []string, map[string]providers.UsageInfo, error) {
	iteration := 0
	var finalContent string
	var sentMessages []string
	totalUsage := make(map[string]providers.UsageInfo)
	activeModel := inst.Model

	for {
		iteration++
		if iteration > inst.MaxIterations {
			log.Ctx(ctx).Warn().Int("iterations", iteration).Msg("Max iterations reached")
			break
		}

		// 1. Prepare and Call LLM
		iterMessages := d.prepareIterationMessages(ctx, messages)
		response, err := inst.Provider.Chat(ctx, iterMessages, inst.Tools.ToProviderDefs(), activeModel, map[string]any{
			"max_tokens":       inst.MaxTokens,
			"temperature":      inst.Temperature,
			"prompt_cache_key": inst.ID,
		})
		if err != nil {
			return "", iteration, sentMessages, totalUsage, fmt.Errorf("LLM call failed: %w", err)
		}

		// 2. Track usage and handle reasoning
		d.trackUsage(ctx, response.Usage, totalUsage, activeModel, iteration, opts.ChatID)
		d.handleReasoning(ctx, response.Reasoning, opts)

		// 3. Termination check
		if len(response.ToolCalls) == 0 {
			finalContent = response.Content
			if finalContent == "" && response.ReasoningContent != "" {
				finalContent = response.ReasoningContent
			}
			break
		}

		// 4. Dispatch and execute tools
		batch := d.dispatchTools(ctx, inst, response, opts, iteration)

		// 5. Save assistant message and update local state
		messages = append(messages, batch.assistantMsg)
		if !opts.DisableHistory {
			inst.Sessions.AddFullMessage(opts.SessionKey, batch.assistantMsg)
		}

		// 6. Process tool outputs
		d.applyToolResults(ctx, inst, batch.results, &messages, &sentMessages, opts)

		inst.Tools.TickTTL()
	}

	return finalContent, iteration, sentMessages, totalUsage, nil
}

func (d *Driver) prepareIterationMessages(ctx context.Context, messages []providers.Message) []providers.Message {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)

		var timeStr string
		if remaining > time.Minute {
			// Round to the nearest minute to keep the prompt stable for caching
			timeStr = fmt.Sprintf("%d minutes", int(remaining.Round(time.Minute).Minutes()))
		} else {
			timeStr = "less than a minute"
		}

		hint := fmt.Sprintf("System: Remaining time for this task: %s.", timeStr)
		if remaining < 30*time.Second {
			hint += " Time is CRITICAL. Wrap up and provide the final answer NOW."
		} else {
			hint += " Plan your tool calls accordingly."
		}

		iterMessages := append([]providers.Message{}, messages...)
		return append(iterMessages, providers.Message{
			Role:    "system",
			Content: hint,
		})
	}
	return messages
}

func (d *Driver) trackUsage(ctx context.Context, usage *providers.UsageInfo, total map[string]providers.UsageInfo, model string, iter int, chatID string) {
	if usage == nil {
		return
	}
	u := total[model]
	u.PromptTokens += usage.PromptTokens
	u.CompletionTokens += usage.CompletionTokens
	u.TotalTokens += usage.TotalTokens
	total[model] = u

	log.Ctx(ctx).Info().
		Str("model", model).
		Int("iteration", iter).
		Int("prompt_tokens", usage.PromptTokens).
		Int("completion_tokens", usage.CompletionTokens).
		Int("total_tokens", usage.TotalTokens).
		Msg("LLM response received")
}

func (d *Driver) handleReasoning(ctx context.Context, reasoning string, opts processOptions) {
	if reasoning == "" {
		return
	}
	log.Ctx(ctx).Info().Str("chat_id", opts.ChatID).Str("reasoning", reasoning).Msg("Agent reasoning")

	if !opts.SuppressReasoning {
		go func() {
			pubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = d.Bus.PublishOutbound(pubCtx, bus.OutboundMessage{
				Channel: opts.Channel,
				ChatID:  opts.ChatID,
				Content: reasoning,
			})
		}()
	}
}

type toolResultBatch struct {
	assistantMsg providers.Message
	results      []toolExecutionResult
}

type toolExecutionResult struct {
	tc     providers.ToolCall
	result *tools.ToolResult
}

func (d *Driver) dispatchTools(ctx context.Context, inst *agent.AgentInstance, response *providers.LLMResponse, opts processOptions, iter int) toolResultBatch {
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

	for _, tc := range assistantMsg.ToolCalls {
		log.Ctx(ctx).Info().
			Str("chat_id", opts.ChatID).
			Str("tool", tc.Name).
			Str("tool_call_id", tc.ID).
			Str("arguments", tc.Function.Arguments).
			Int("iteration", iter).
			Msg("Tool call")
	}

	batch := toolResultBatch{
		assistantMsg: assistantMsg,
		results:      make([]toolExecutionResult, len(assistantMsg.ToolCalls)),
	}

	var wg sync.WaitGroup
	for i, tc := range assistantMsg.ToolCalls {
		wg.Add(1)
		go func(idx int, t providers.ToolCall) {
			defer wg.Done()
			var args map[string]any
			_ = json.Unmarshal([]byte(t.Function.Arguments), &args)
			toolCtx := tools.WithToolInboundContext(ctx, opts.Channel, opts.ChatID, opts.MessageID, "")
			res := inst.Tools.ExecuteWithContext(toolCtx, t.Name, args, opts.Channel, opts.ChatID, nil)
			batch.results[idx] = toolExecutionResult{tc: t, result: res}
		}(i, tc)
	}
	wg.Wait()
	return batch
}

func (d *Driver) applyToolResults(ctx context.Context, inst *agent.AgentInstance, results []toolExecutionResult, messages *[]providers.Message, sentMessages *[]string, opts processOptions) {
	for _, r := range results {
		log.Ctx(ctx).Info().
			Str("chat_id", opts.ChatID).
			Str("tool", r.tc.Name).
			Str("tool_call_id", r.tc.ID).
			Str("result", r.result.ForLLM).
			Bool("is_error", r.result.IsError).
			Msg("Tool result")

		if r.tc.Name == "message" {
			var args struct {
				Content string `json:"content"`
			}
			_ = json.Unmarshal([]byte(r.tc.Function.Arguments), &args)
			if args.Content != "" {
				*sentMessages = append(*sentMessages, args.Content)
			}
		}

		if !opts.SuppressToolFeedback && !r.result.Silent && r.result.ForUser != "" {
			_ = d.Bus.PublishOutbound(ctx, bus.OutboundMessage{
				Channel: opts.Channel,
				ChatID:  opts.ChatID,
				Content: r.result.ForUser,
			})
		}

		if len(r.result.Media) > 0 {
			d.publishMedia(ctx, opts, r.result.Media)
		}

		contentForLLM := r.result.ForLLM
		if contentForLLM == "" && r.result.Err != nil {
			contentForLLM = r.result.Err.Error()
		}

		toolResultMsg := providers.Message{
			Role:       "tool",
			Content:    contentForLLM,
			ToolCallID: r.tc.ID,
		}
		*messages = append(*messages, toolResultMsg)
		if !opts.DisableHistory {
			inst.Sessions.AddFullMessage(opts.SessionKey, toolResultMsg)
		}
	}
}

func (d *Driver) publishMedia(ctx context.Context, opts processOptions, mediaRefs []string) {
	parts := make([]bus.MediaPart, 0, len(mediaRefs))
	for _, ref := range mediaRefs {
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
