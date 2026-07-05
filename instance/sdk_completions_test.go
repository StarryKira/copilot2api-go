package instance

import (
	"encoding/json"
	"strings"
	"testing"

	"copilot-go/anthropic"
)

type parsedTool struct {
	idx  int
	id   string
	name string
	args string
}

// drive feeds the given text fragments through a single mcpParser, returning the
// concatenated plain text and the parsed tool calls.
func drive(fragments []string) (string, []parsedTool, bool) {
	p := &mcpParser{}
	var text strings.Builder
	var tools []parsedTool
	onText := func(s string) { text.WriteString(s) }
	onTool := func(idx int, id, name, args string) {
		tools = append(tools, parsedTool{idx, id, name, args})
	}
	for _, f := range fragments {
		p.feed(f, onText, onTool)
	}
	p.flushRemaining(onText, onTool)
	return text.String(), tools, p.sawTool
}

func TestParserSingleToolCall(t *testing.T) {
	in := `[mcp.result]{"name":"get_weather","arguments":{"city":"Paris"}}[/mcp.result]`
	text, tools, saw := drive([]string{in})
	if !saw {
		t.Fatal("expected sawTool=true")
	}
	if text != "" {
		t.Fatalf("expected no plain text, got %q", text)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].name != "get_weather" {
		t.Fatalf("name = %q", tools[0].name)
	}
	if tools[0].args != `{"city":"Paris"}` {
		t.Fatalf("args = %q", tools[0].args)
	}
	if tools[0].id != "call_0" {
		t.Fatalf("id = %q", tools[0].id)
	}
}

func TestParserSplitAcrossChunks(t *testing.T) {
	full := `prefix text [mcp.result]{"name":"f","arguments":{"a":1}}[/mcp.result] tail`
	// Feed one byte at a time to stress the streaming boundaries.
	frags := make([]string, 0, len(full))
	for _, r := range full {
		frags = append(frags, string(r))
	}
	text, tools, saw := drive(frags)
	if !saw || len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d (saw=%v)", len(tools), saw)
	}
	if tools[0].name != "f" || tools[0].args != `{"a":1}` {
		t.Fatalf("unexpected tool %+v", tools[0])
	}
	if strings.Contains(text, "mcp.result") || strings.Contains(text, "[") {
		t.Fatalf("marker leaked into text: %q", text)
	}
	if !strings.Contains(text, "prefix text") || !strings.Contains(text, "tail") {
		t.Fatalf("surrounding text lost: %q", text)
	}
}

func TestParserMultipleToolCalls(t *testing.T) {
	in := `[mcp.result]{"name":"a","arguments":{}}[/mcp.result]` +
		`[mcp.result]{"name":"b","arguments":{"x":true}}[/mcp.result]`
	_, tools, _ := drive([]string{in})
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].name != "a" || tools[0].idx != 0 || tools[0].id != "call_0" {
		t.Fatalf("tool0 = %+v", tools[0])
	}
	if tools[1].name != "b" || tools[1].idx != 1 || tools[1].id != "call_1" {
		t.Fatalf("tool1 = %+v", tools[1])
	}
}

func TestParserPlainTextNoLeak(t *testing.T) {
	in := "Just a normal answer with brackets [like] this."
	text, tools, saw := drive([]string{in})
	if saw || len(tools) != 0 {
		t.Fatalf("did not expect tools, got %d", len(tools))
	}
	if text != in {
		t.Fatalf("text mismatch: %q", text)
	}
}

func TestParserComplexArgsPreserved(t *testing.T) {
	args := `{"path":"a/b.go","nested":{"arr":[1,2,3],"s":"he said \"hi\""}}`
	in := `[mcp.result]{"name":"edit","arguments":` + args + `}[/mcp.result]`
	_, tools, _ := drive([]string{in})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	// Re-marshal both sides to compare semantically (key order independent).
	var got, want interface{}
	if err := json.Unmarshal([]byte(tools[0].args), &got); err != nil {
		t.Fatalf("args not valid json: %v (%q)", err, tools[0].args)
	}
	if err := json.Unmarshal([]byte(args), &want); err != nil {
		t.Fatal(err)
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("args mismatch: got %s want %s", g, w)
	}
}

func TestParserUnterminatedBlockSalvagedAsTool(t *testing.T) {
	// Models often stop right after the JSON without the close tag.
	cases := []string{
		`[mcp.result]{"name":"get_weather","arguments":{"city":"Paris"}}`,
		`[mcp.result]{"name":"get_weather","arguments":{"city":"Paris"}}` + "\n",
		`[mcp.result]{"name":"get_weather","arguments":{"city":"Paris"}}[/mcp`,
	}
	for _, in := range cases {
		text, tools, saw := drive([]string{in})
		if !saw || len(tools) != 1 {
			t.Fatalf("input %q: expected salvaged tool, got tools=%d text=%q", in, len(tools), text)
		}
		if tools[0].name != "get_weather" || tools[0].args != `{"city":"Paris"}` {
			t.Fatalf("input %q: unexpected tool %+v", in, tools[0])
		}
		if text != "" {
			t.Fatalf("input %q: expected no text, got %q", in, text)
		}
	}
}

func TestParserUnterminatedInvalidBlockKeptAsText(t *testing.T) {
	// Truncated JSON (e.g. max_tokens cutoff) must fall back to text.
	in := `[mcp.result]{"name":"get_wea`
	text, tools, _ := drive([]string{in})
	if len(tools) != 0 {
		t.Fatalf("expected no tools, got %+v", tools)
	}
	if text != in {
		t.Fatalf("expected raw text back, got %q", text)
	}
}

func TestParserInvalidToolBlockSurfacedAsText(t *testing.T) {
	in := `[mcp.result]this is not json[/mcp.result]`
	text, tools, saw := drive([]string{in})
	if saw || len(tools) != 0 {
		t.Fatalf("did not expect tools, got %d (saw=%v)", len(tools), saw)
	}
	if text != in {
		t.Fatalf("invalid block should be surfaced verbatim, got %q", text)
	}
}

func TestParserEmptyArgsDefaultsToObject(t *testing.T) {
	in := `[mcp.result]{"name":"noargs"}[/mcp.result]`
	_, tools, _ := drive([]string{in})
	if len(tools) != 1 || tools[0].args != "{}" {
		t.Fatalf("expected args {}, got %+v", tools)
	}
}

// streamToAnthropic mimics sdkStream: parse assistant text into synthetic OpenAI
// chunks, then run them through the production stream translator. Returns the
// concatenated JSON of all emitted Anthropic SSE events.
func streamToAnthropic(t *testing.T, chunks []string) string {
	t.Helper()
	p := &mcpParser{}
	var raw []string
	writeText := func(s string) {
		if s == "" {
			return
		}
		raw = append(raw, marshalChunk("m", s, ""))
	}
	writeTool := func(idx int, id, name, args string) {
		raw = append(raw, marshalToolChunk("m", idx, id, name, args))
	}
	for _, c := range chunks {
		p.feed(c, writeText, writeTool)
	}
	p.flushRemaining(writeText, writeTool)
	finish := "stop"
	if p.sawTool {
		finish = "tool_calls"
	}
	raw = append(raw, marshalChunk("m", "", finish))

	state := anthropic.NewStreamState()
	var sb strings.Builder
	for _, data := range raw {
		var chunk anthropic.ChatCompletionResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("synthetic chunk not parseable: %v (%s)", err, data)
		}
		for _, ev := range anthropic.TranslateChunkToAnthropicEvents(chunk, state) {
			b, _ := json.Marshal(ev.Data)
			sb.Write(b)
		}
	}
	return sb.String()
}

func TestStreamEndToEndToolUse(t *testing.T) {
	in := `Sure.[mcp.result]{"name":"get_weather","arguments":{"city":"Paris"}}[/mcp.result]`
	out := streamToAnthropic(t, []string{in})
	for _, want := range []string{
		`"type":"tool_use"`,
		`"name":"get_weather"`,
		`"type":"input_json_delta"`,
		`Paris`,
		`"stop_reason":"tool_use"`,
		`"type":"text_delta"`, // the leading "Sure." text
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream output missing %q\nfull: %s", want, out)
		}
	}
}

func TestStreamPlainTextStopReason(t *testing.T) {
	out := streamToAnthropic(t, []string{"hello world"})
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("expected end_turn stop reason, got: %s", out)
	}
	if strings.Contains(out, "tool_use") {
		t.Fatalf("unexpected tool_use in plain text stream: %s", out)
	}
}

func TestRenderAssistantToolCallsRoundTrip(t *testing.T) {
	// OpenAI-format assistant tool_calls (as raw maps) should render back into
	// the same protocol blocks the model is taught to emit.
	raw := []interface{}{
		map[string]interface{}{
			"id":   "call_0",
			"type": "function",
			"function": map[string]interface{}{
				"name":      "get_weather",
				"arguments": `{"city":"Paris"}`,
			},
		},
	}
	got := renderAssistantToolCalls(raw)
	if !strings.HasPrefix(got, mcpOpenTag) || !strings.HasSuffix(got, mcpCloseTag) {
		t.Fatalf("unexpected render: %q", got)
	}
	// And it should parse back to the same tool call.
	_, tools, _ := drive([]string{got})
	if len(tools) != 1 || tools[0].name != "get_weather" || tools[0].args != `{"city":"Paris"}` {
		t.Fatalf("round trip failed: %+v", tools)
	}
}

func TestBuildToolInstructionsMentionsTools(t *testing.T) {
	tools := []anthropic.OpenAITool{
		{Type: "function", Function: anthropic.OpenAIFunction{
			Name: "read_file", Description: "Read a file",
			Parameters: map[string]interface{}{"type": "object"},
		}},
	}
	instr := buildToolInstructions(tools)
	for _, want := range []string{"read_file", "Read a file", mcpOpenTag, "input schema"} {
		if !strings.Contains(instr, want) {
			t.Fatalf("instructions missing %q:\n%s", want, instr)
		}
	}
}
