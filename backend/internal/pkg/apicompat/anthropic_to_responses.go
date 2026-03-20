package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AnthropicToResponses converts an Anthropic Messages request directly into
// a Responses API request. This preserves fields that would be lost in a
// Chat Completions intermediary round-trip (e.g. thinking, cache_control,
// structured system prompts).
func AnthropicToResponses(req *AnthropicRequest) (*ResponsesRequest, error) {
	input, err := convertAnthropicToResponsesInput(req.System, req.Messages)
	if err != nil {
		return nil, err
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}

	out := &ResponsesRequest{
		Model:       req.Model,
		Input:       inputJSON,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Include:     []string{"reasoning.encrypted_content"},
	}

	storeFalse := false
	out.Store = &storeFalse

	if req.MaxTokens > 0 {
		v := req.MaxTokens
		if v < minMaxOutputTokens {
			v = minMaxOutputTokens
		}
		out.MaxOutputTokens = &v
	}

	if len(req.Tools) > 0 {
		out.Tools = convertAnthropicToolsToResponses(req.Tools)
	}

	// Determine reasoning effort.
	// Priority: output_config.effort > thinking.enabled budget mapping > default "high" (→ xhigh).
	// thinking.disabled does NOT change the effort level (keeps default).
	effort := "high" // default → xhigh
	if req.OutputConfig != nil && req.OutputConfig.Effort != "" {
		effort = req.OutputConfig.Effort
	} else if req.Thinking != nil && req.Thinking.Type == "enabled" {
		if e := thinkingBudgetToEffort(req.Thinking.BudgetTokens); e != "" {
			effort = e
		}
	}
	out.Reasoning = &ResponsesReasoning{
		Effort:  mapAnthropicEffortToResponses(effort),
		Summary: "auto",
	}

	// Convert tool_choice
	if len(req.ToolChoice) > 0 {
		tc, err := convertAnthropicToolChoiceToResponses(req.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("convert tool_choice: %w", err)
		}
		out.ToolChoice = tc
	}

	return out, nil
}

// convertAnthropicToolChoiceToResponses maps Anthropic tool_choice to Responses format.
//
//	{"type":"auto"}            → "auto"
//	{"type":"any"}             → "required"
//	{"type":"none"}            → "none"
//	{"type":"tool","name":"X"} → {"type":"function","function":{"name":"X"}}
func convertAnthropicToolChoiceToResponses(raw json.RawMessage) (json.RawMessage, error) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, err
	}

	switch tc.Type {
	case "auto":
		return json.Marshal("auto")
	case "any":
		return json.Marshal("required")
	case "none":
		return json.Marshal("none")
	case "tool":
		return json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		})
	default:
		// Pass through unknown types as-is
		return raw, nil
	}
}

// convertAnthropicToResponsesInput builds the Responses API input items array
// from the Anthropic system field and message list.
func convertAnthropicToResponsesInput(system json.RawMessage, msgs []AnthropicMessage) ([]ResponsesInputItem, error) {
	var out []ResponsesInputItem

	// System prompt → system role input item.
	if len(system) > 0 {
		sysText, err := parseAnthropicSystemPrompt(system)
		if err != nil {
			return nil, err
		}
		if sysText != "" {
			content, _ := json.Marshal(sysText)
			out = append(out, ResponsesInputItem{
				Role:    "system",
				Content: content,
			})
		}
	}

	for _, m := range msgs {
		items, err := anthropicMsgToResponsesItems(m)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// parseAnthropicSystemPrompt handles the Anthropic system field which can be
// a plain string or an array of text blocks.
func parseAnthropicSystemPrompt(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// anthropicMsgToResponsesItems converts a single Anthropic message into one
// or more Responses API input items.
func anthropicMsgToResponsesItems(m AnthropicMessage) ([]ResponsesInputItem, error) {
	switch m.Role {
	case "user":
		return anthropicUserToResponses(m.Content)
	case "assistant":
		return anthropicAssistantToResponses(m.Content)
	default:
		return anthropicUserToResponses(m.Content)
	}
}

// anthropicUserToResponses handles an Anthropic user message. Content can be a
// plain string or an array of blocks. tool_result blocks are extracted into
// function_call_output items. Image blocks are converted to input_image parts.
func anthropicUserToResponses(raw json.RawMessage) ([]ResponsesInputItem, error) {
	// Try plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		content, _ := json.Marshal(s)
		return []ResponsesInputItem{{Role: "user", Content: content}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	var out []ResponsesInputItem
	var toolResultImageParts []ResponsesContentPart

	// Extract tool_result blocks → function_call_output items.
	// Images inside tool_results are extracted separately because the
	// Responses API function_call_output.output only accepts strings.
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		outputText, imageParts := convertToolResultOutput(b)
		out = append(out, ResponsesInputItem{
			Type:   "function_call_output",
			CallID: toResponsesCallID(b.ToolUseID),
			Output: outputText,
		})
		toolResultImageParts = append(toolResultImageParts, imageParts...)
	}

	// Remaining text + image blocks → user message with content parts.
	// Also include images extracted from tool_results so the model can see them.
	var parts []ResponsesContentPart
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, ResponsesContentPart{Type: "input_text", Text: b.Text})
			}
		case "image":
			if uri := anthropicImageToDataURI(b.Source); uri != "" {
				parts = append(parts, ResponsesContentPart{Type: "input_image", ImageURL: uri})
			}
		}
	}
	parts = append(parts, toolResultImageParts...)

	if len(parts) > 0 {
		content, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		out = append(out, ResponsesInputItem{Role: "user", Content: content})
	}

	return out, nil
}

// anthropicAssistantToResponses handles an Anthropic assistant message.
// Text content → assistant message with output_text parts.
// tool_use blocks → function_call items.
// thinking blocks → ignored (OpenAI doesn't accept them as input).
func anthropicAssistantToResponses(raw json.RawMessage) ([]ResponsesInputItem, error) {
	// Try plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		parts := []ResponsesContentPart{{Type: "output_text", Text: s}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		return []ResponsesInputItem{{Role: "assistant", Content: partsJSON}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	var items []ResponsesInputItem

	// Text content → assistant message with output_text content parts.
	text := extractAnthropicTextFromBlocks(blocks)
	if text != "" {
		parts := []ResponsesContentPart{{Type: "output_text", Text: text}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		items = append(items, ResponsesInputItem{Role: "assistant", Content: partsJSON})
	}

	// tool_use → function_call items.
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		args := "{}"
		if len(b.Input) > 0 {
			args = string(b.Input)
		}
		fcID := toResponsesCallID(b.ID)
		items = append(items, ResponsesInputItem{
			Type:      "function_call",
			CallID:    fcID,
			Name:      b.Name,
			Arguments: args,
		})
	}

	return items, nil
}

// toResponsesCallID converts an Anthropic tool ID (toolu_xxx / call_xxx) to a
// Responses API function_call ID that starts with "fc_".
func toResponsesCallID(id string) string {
	if strings.HasPrefix(id, "fc_") {
		return id
	}
	return "fc_" + id
}

// fromResponsesCallID reverses toResponsesCallID, stripping the "fc_" prefix
// that was added during request conversion.
func fromResponsesCallID(id string) string {
	if after, ok := strings.CutPrefix(id, "fc_"); ok {
		// Only strip if the remainder doesn't look like it was already "fc_" prefixed.
		// E.g. "fc_toolu_xxx" → "toolu_xxx", "fc_call_xxx" → "call_xxx"
		if strings.HasPrefix(after, "toolu_") || strings.HasPrefix(after, "call_") {
			return after
		}
	}
	return id
}

// anthropicImageToDataURI converts an AnthropicImageSource to a data URI string.
// Returns "" if the source is nil or has no data.
func anthropicImageToDataURI(src *AnthropicImageSource) string {
	if src == nil || src.Data == "" {
		return ""
	}
	mediaType := src.MediaType
	if mediaType == "" {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + src.Data
}

// convertToolResultOutput extracts text and image content from a tool_result
// block. Returns the text as a string for the function_call_output Output
// field, plus any image parts that must be sent in a separate user message
// (the Responses API output field only accepts strings).
func convertToolResultOutput(b AnthropicContentBlock) (string, []ResponsesContentPart) {
	if len(b.Content) == 0 {
		return "(empty)", nil
	}

	// Try plain string content.
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		if s == "" {
			s = "(empty)"
		}
		return s, nil
	}

	// Array of content blocks — may contain text and/or images.
	var inner []AnthropicContentBlock
	if err := json.Unmarshal(b.Content, &inner); err != nil {
		return "(empty)", nil
	}

	// Separate text (for function_call_output) from images (for user message).
	var textParts []string
	var imageParts []ResponsesContentPart
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			if ib.Text != "" {
				textParts = append(textParts, ib.Text)
			}
		case "image":
			if uri := anthropicImageToDataURI(ib.Source); uri != "" {
				imageParts = append(imageParts, ResponsesContentPart{Type: "input_image", ImageURL: uri})
			}
		}
	}

	text := strings.Join(textParts, "\n\n")
	if text == "" {
		text = "(empty)"
	}
	return text, imageParts
}

// extractAnthropicTextFromBlocks joins all text blocks, ignoring thinking/
// tool_use/tool_result blocks.
func extractAnthropicTextFromBlocks(blocks []AnthropicContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// mapAnthropicEffortToResponses converts Anthropic reasoning effort levels to
// OpenAI Responses API effort levels.
//
//	low    → low
//	medium → high
//	high   → xhigh
func mapAnthropicEffortToResponses(effort string) string {
	switch effort {
	case "medium":
		return "high"
	case "high":
		return "xhigh"
	default:
		return effort // "low" and any unknown values pass through unchanged
	}
}

// convertAnthropicToolsToResponses maps Anthropic tool definitions to
// Responses API tools. Server-side tools like web_search are mapped to their
// OpenAI equivalents; regular tools become function tools.
// Tool names longer than 64 characters (e.g. MCP tools like mcp__server__method)
// are shortened to meet the Responses API limit.
func convertAnthropicToolsToResponses(tools []AnthropicTool) []ResponsesTool {
	// Build a unique short-name map for the whole request to avoid collisions.
	var names []string
	for _, t := range tools {
		if !strings.HasPrefix(t.Type, "web_search") {
			names = append(names, t.Name)
		}
	}
	shortMap := buildToolShortNameMap(names)

	var out []ResponsesTool
	for _, t := range tools {
		// Anthropic server tools like "web_search_20250305" → OpenAI {"type":"web_search"}
		if strings.HasPrefix(t.Type, "web_search") {
			out = append(out, ResponsesTool{Type: "web_search"})
			continue
		}
		shortName := shortMap[t.Name]
		out = append(out, ResponsesTool{
			Type:        "function",
			Name:        shortName,
			Description: t.Description,
			Parameters:  normalizeToolParameters(t.InputSchema),
		})
	}
	return out
}

// buildToolShortNameMap builds a map from original tool name → shortened name,
// guaranteeing uniqueness within the request. Names ≤ 64 chars are unchanged.
// MCP tools (mcp__server__method) are shortened to mcp__method; others are
// truncated. Conflicts are resolved by appending _1, _2, etc.
func buildToolShortNameMap(names []string) map[string]string {
	const limit = 64
	m := make(map[string]string, len(names))
	used := make(map[string]struct{}, len(names))

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) <= limit {
					return cand
				}
				return cand[:limit]
			}
		}
		return n[:limit]
	}

	for _, n := range names {
		cand := baseCandidate(n)
		if _, taken := used[cand]; !taken {
			m[n] = cand
			used[cand] = struct{}{}
			continue
		}
		// Resolve collision
		for i := 1; ; i++ {
			suffix := fmt.Sprintf("_%d", i)
			allowed := limit - len(suffix)
			base := cand
			if len(base) > allowed {
				base = base[:allowed]
			}
			uniq := base + suffix
			if _, taken := used[uniq]; !taken {
				m[n] = uniq
				used[uniq] = struct{}{}
				break
			}
		}
	}
	return m
}

// thinkingBudgetToEffort maps Anthropic thinking budget_tokens to a Responses
// API reasoning effort level. Mirrors airgate-openai thinkingBudgetToReasoningEffort.
func thinkingBudgetToEffort(budget int) string {
	switch {
	case budget == 0:
		return "none"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	default:
		return "xhigh"
	}
}

// BuildReverseToolNameMap builds a short→original map from an Anthropic tools list.
// Used on the response side to restore shortened names back to the originals.
func BuildReverseToolNameMap(tools []AnthropicTool) map[string]string {
	var names []string
	for _, t := range tools {
		if !strings.HasPrefix(t.Type, "web_search") {
			names = append(names, t.Name)
		}
	}
	forward := buildToolShortNameMap(names)
	rev := make(map[string]string, len(forward))
	for orig, short := range forward {
		rev[short] = orig
	}
	return rev
}

// normalizeToolParameters ensures the tool parameter schema is valid for
// OpenAI's Responses API, which requires "properties" on object schemas.
//
//   - nil/empty → {"type":"object","properties":{}}
//   - type=object without properties → adds "properties": {}
//   - if type is missing → sets type=object and properties={}
//   - otherwise → returned unchanged
func normalizeToolParameters(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 || string(schema) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}

	// Check if type=object and properties is missing (and add defaults when type is absent).
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(schema, &obj); err != nil {
		return schema
	}

	typeVal, hasType := obj["type"]
	if !hasType {
		obj["type"] = json.RawMessage(`"object"`)
		obj["properties"] = json.RawMessage(`{}`)
	} else {
		var typStr string
		if json.Unmarshal(typeVal, &typStr) == nil && typStr == "object" {
			if _, hasProps := obj["properties"]; !hasProps {
				obj["properties"] = json.RawMessage(`{}`)
			}
		}
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return schema
	}
	return out
}
