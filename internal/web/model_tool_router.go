package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"m365-copilot2api/internal/chathub"
)

// runToolRouter asks the upstream model to pick the next tool through a
// throwaway router conversation, with one repair round on unparsable output.
// Router and repair conversations are deleted after use so the conversation
// list does not accumulate one entry per routed request.
//
// A disengaged router (typical with large tool catalogs) returns
// (nil, zero Result, false, nil) so the caller can fall back to an ordinary
// answer instead of failing the whole request; other router failures return
// an error already prefixed with "tool router:".
func (s *Server) runToolRouter(ctx context.Context, account chathub.Account, tone, traceID, routeContext string, routeMaps []map[string]any, toolChoice any, attachments []chathub.Attachment, ledger agentLedger) ([]detectedToolCall, chathub.Result, bool, error) {
	routePrompt := modelToolRouterPrompt(routeContext, routeMaps, toolChoice)
	log.Printf("[req-trace] id=%s stage=router_start prompt_len=%d", traceID, len(routePrompt))
	routeRes, routeErr := s.chat.Chat(ctx, account, chathub.Request{Text: routePrompt, Tone: tone, Attachments: attachments})
	if routeErr == nil && routeRes.ConversationID != "" {
		s.dropTransientConversation(routeRes.ConversationID)
	}
	if routeErr != nil {
		if IsDisengaged(routeErr) {
			log.Printf("[req-trace] id=%s stage=router_disengaged fallback=plain_answer", traceID)
			return nil, chathub.Result{}, false, nil
		}
		return nil, chathub.Result{}, false, fmt.Errorf("tool router: %w", routeErr)
	}
	calls, parsed := parseModelToolDecision(routeRes.Text, routeMaps, toolChoice)
	calls = filterCompletedCalls(calls, ledger)
	if !parsed {
		repairRes, repairErr := s.chat.Chat(ctx, account, chathub.Request{
			Text:        repairRouterPrompt(routeRes.Text),
			Tone:        tone,
			Attachments: attachments,
		})
		if repairErr == nil && repairRes.ConversationID != "" {
			s.dropTransientConversation(repairRes.ConversationID)
		}
		if repairErr == nil {
			calls, parsed = parseModelToolDecision(repairRes.Text, routeMaps, toolChoice)
			calls = filterCompletedCalls(calls, ledger)
		}
	}
	return calls, routeRes, parsed, nil
}

// repairRouterPrompt asks the model to reformat an unparsable routing
// decision as JSON only.
func repairRouterPrompt(raw string) string {
	return `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Use {"calls":[]} if no tool is needed. OUTPUT:
` + compactToolResult(raw, 6000)
}

// finalizeToolCalls assigns stable scoped call IDs (scope "" keeps existing
// IDs) and applies the adaptive parallel-call limit.
func (s *Server) finalizeToolCalls(calls []detectedToolCall, scope string) []detectedToolCall {
	if scope != "" {
		for i := range calls {
			calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
		}
	}
	return limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
}

func modelToolRouterPrompt(prompt string, tools []map[string]any, choice any) string {
	defs, _ := json.Marshal(tools)
	mode := normalizedToolChoiceMode(choice)
	rules := `- If a tool is needed, respond with: CALL_TOOL: tool_name({"arg1":"value1"})
- If no tool is needed, respond with: NO_TOOL_NEEDED
- Only use tools from the available list above
- Validate all arguments against the tool's schema
- Do not invent tools that are not in the list`
	// Multi-turn: completed tool evidence (tool[...], tool_calls:) was already
	// acted upon, so re-invoking those tools would duplicate work.
	if strings.Contains(prompt, "tool_calls:") || strings.Contains(prompt, "tool[call_") {
		rules += `
- Completed evidence must not be repeated: tool_calls/tool[call_x] rows are prior results already delivered to the user, never re-invoke them
- Only start a new tool call when fresh unfinished work remains on the current request`
	}
	return fmt.Sprintf(`You are a tool selection assistant. Based on the user request, decide which tool to call next.

Available tools: %s

MODE: %s

Rules:
%s

User request and evidence:
%s`, defs, mode, rules, prompt)
}

func parseModelToolDecision(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	text = strings.TrimSpace(text)
	// Try the new natural language format first: CALL_TOOL: name({...})
	if strings.HasPrefix(text, "CALL_TOOL:") || strings.HasPrefix(text, "call_tool:") {
		parts := strings.SplitN(text, ":", 2)
		if len(parts) == 2 {
			rest := strings.TrimSpace(parts[1])
			start := strings.Index(rest, "(")
			end := strings.LastIndex(rest, ")")
			if start > 0 && end > start {
				name := strings.TrimSpace(rest[:start])
				argsStr := rest[start+1 : end]
				var args map[string]any
				if json.Unmarshal([]byte(argsStr), &args) == nil && toolChoiceAllows(choice, name) {
					fn := toolFunction(name, tools)
					if fn != nil {
						b, _ := json.Marshal(args)
						return []detectedToolCall{{ID: callID(name, string(b), 0), Type: toolType(name, tools), Name: name, Arguments: b}}, true
					}
				}
			}
		}
	}
	if strings.Contains(text, "NO_TOOL_NEEDED") || strings.Contains(text, "no_tool_needed") {
		return nil, true
	}
	// Fallback: try the old JSON format
	if i := strings.Index(text, "```"); i >= 0 {
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(text[i+3:], "```"), "json"))
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	var envelope struct {
		Calls []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"calls"`
	}
	if json.Unmarshal([]byte(text[start:end+1]), &envelope) != nil {
		return nil, false
	}
	out := make([]detectedToolCall, 0, len(envelope.Calls))
	for i, c := range envelope.Calls {
		fn := toolFunction(c.Name, tools)
		if fn == nil || c.Arguments == nil || !toolChoiceAllows(choice, c.Name) || schemaValid(c.Arguments, fn) != nil {
			continue
		}
		b, _ := json.Marshal(c.Arguments)
		out = append(out, detectedToolCall{ID: callID(c.Name, string(b), i), Type: toolType(c.Name, tools), Name: c.Name, Arguments: b})
	}
	return out, true
}
