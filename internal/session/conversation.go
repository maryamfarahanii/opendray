package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// conversation.go backs the Claude-style chat page: it exposes a
// session's structured transcript (instead of the TUI's cell grid, which
// cannot render RTL or joined Persian letters) plus a tiny per-user
// metadata store for chat titles and topics.

const (
	conversationMaxMessages = 400
	conversationToolCap     = 2000
	conversationActiveSpan  = 4 * time.Second
	conversationStaleSpan   = 30 * time.Second
)

// ConversationBlock is one renderable piece of a message.
type ConversationBlock struct {
	Type    string `json:"type"` // text | tool_use | tool_result
	Text    string `json:"text,omitempty"`
	Name    string `json:"name,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Desc and Target let the page word a tool call in plain language.
	Desc    string `json:"desc,omitempty"`
	Target  string `json:"target,omitempty"`
	ToolID  string `json:"tool_id,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// Ask holds AskUserQuestion's questions, so the chat can render the
	// form itself: the TUI's tabbed dialog can't be read off the screen.
	Ask json.RawMessage `json:"ask,omitempty"`
}

// ConversationMessage is one transcript line reduced for display.
type ConversationMessage struct {
	ID     string              `json:"id"`
	Role   string              `json:"role"` // user | assistant
	Ts     time.Time           `json:"ts"`
	Blocks []ConversationBlock `json:"blocks"`
}

// ConversationResponse is the body of GET /sessions/{id}/conversation.
type ConversationResponse struct {
	Messages []ConversationMessage `json:"messages"`
	// Working: the agent still owes a reply (the tail is a user turn or
	// an unresolved tool call) or the transcript was written just now.
	Working bool `json:"working"`
	// PendingTool: the tail is a tool call without a result, which on an
	// idle session means the TUI is waiting on an approval.
	PendingTool bool `json:"pending_tool"`
	// Ready: the TUI's input box is on screen, so input will be accepted.
	Ready bool `json:"ready"`
	// Prompt is a menu the TUI is waiting on (folder trust, a permission
	// request, a question), read off the rendered screen.
	Prompt *ScreenPrompt `json:"prompt,omitempty"`
	// Activity is the TUI's live status line while it works.
	Activity string `json:"activity,omitempty"`
	// LimitNote is the CLI's usage warning, e.g. "used 98% of your weekly limit".
	LimitNote string `json:"limit_note,omitempty"`
	// Mode is the permission mode on the status line, once Ready.
	Mode string `json:"mode,omitempty"`
	// InUseElsewhere: a stopped chat whose transcript is still being
	// written, i.e. another client has it open. Resuming would race it.
	InUseElsewhere bool `json:"in_use_elsewhere,omitempty"`
	Unsupported    bool `json:"unsupported,omitempty"`
}

type conversationLine struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	Time        time.Time       `json:"timestamp"`
	IsMeta      bool            `json:"isMeta"`
	IsSidechain bool            `json:"isSidechain"`
	IsCompact   bool            `json:"isCompactSummary"`
	Message     *claudeJSONLMsg `json:"message"`
	// Attachment carries a message the operator sent while Claude was
	// mid-turn: Claude Code records it as a queued_command attachment,
	// never as a plain user line.
	Attachment *struct {
		Type   string `json:"type"`
		Prompt string `json:"prompt"`
	} `json:"attachment"`
}

// Conversation returns the session's transcript as display messages.
func (m *Manager) Conversation(ctx context.Context, id string) (ConversationResponse, error) {
	sess, err := m.Get(ctx, id)
	if err != nil {
		return ConversationResponse{}, err
	}
	out := ConversationResponse{Messages: []ConversationMessage{}}
	if sess.ProviderID != "claude" {
		out.Unsupported = true
		return out, nil
	}
	screen := m.RecentScreen(id)
	if screen != "" {
		out.Prompt = parseScreenPrompt(screen)
		out.Ready = out.Prompt == nil && screenInputReady(screen)
		if out.Ready {
			out.Mode = screenPermissionMode(screen)
		}
		if m := screenActivityRe.FindString(screen); m != "" {
			out.Activity = strings.TrimSpace(m)
		}
		out.LimitNote = screenLimitNoteRe.FindString(screen)
	}
	path := conversationTranscriptPath(m.claudeHistoryCfg, sess.EffectiveWorkDir(), sess.ClaudeSessionID)
	if path == "" {
		return out, nil
	}
	msgs, err := readConversation(path)
	if err != nil {
		return out, nil
	}
	out.Messages = msgs
	if n := len(msgs); n > 0 {
		last := msgs[n-1]
		resolved := map[string]bool{}
		for _, msg := range msgs {
			for _, b := range msg.Blocks {
				if b.Type == "tool_result" {
					resolved[b.ToolID] = true
				}
			}
		}
		for _, b := range last.Blocks {
			if b.Type == "tool_use" && !resolved[b.ToolID] {
				out.PendingTool = true
			}
		}
		out.Working = last.Role == "user" || out.PendingTool
	}
	st, err := os.Stat(path)
	if err != nil {
		return out, nil
	}
	if sess.State.IsTerminal() {
		out.Working = false
		out.PendingTool = false
		out.InUseElsewhere = time.Since(st.ModTime()) < oldChatQuietSpan
		return out, nil
	}
	// A turn cut off mid-tool (process restart, crash) leaves a user
	// line or an unresolved tool call at the tail forever. If the
	// terminal shows no spinner and the transcript has gone quiet, the
	// turn is over whatever the tail says.
	if out.Working && screen != "" && !screenBusy(screen) && !screenAwaitsAnswer(screen) && time.Since(st.ModTime()) > conversationStaleSpan {
		out.Working = false
		out.PendingTool = false
	}
	if time.Since(st.ModTime()) < conversationActiveSpan {
		out.Working = true
	}
	return out, nil
}

// screenAwaitsAnswer reports a TUI dialog (AskUserQuestion, permission
// prompt) waiting on the operator; that turn is paused, not over.
func screenAwaitsAnswer(screen string) bool {
	return strings.Contains(screen, "Esc to cancel") || strings.Contains(screen, "Enter to select") || screenFooterRe.MatchString(screen)
}

// screenBusy reports whether the TUI shows Claude mid-turn.
func screenBusy(screen string) bool {
	return screenActivityRe.MatchString(screen) || strings.Contains(screen, "esc to interrupt")
}

// ScreenPrompt is a TUI selection menu.
type ScreenPrompt struct {
	Title    string   `json:"title"`
	Options  []string `json:"options"`
	Selected int      `json:"selected"`
	Trust    bool     `json:"trust,omitempty"`
}

var (
	screenFooterRe    = regexp.MustCompile(`(?i)esc to (cancel|exit)|enter to (confirm|select)`)
	screenActivityRe  = regexp.MustCompile(`(?m)[✻✽✶✢✳·*]\s+[A-Z][a-zA-Z]+…\s*(\([^\n)]*\))?`)
	screenLimitNoteRe = regexp.MustCompile(`You've used \d+% of your [^\n]*`)
	screenRuleRe      = regexp.MustCompile(`^[─━\-=╌\s]*$`)
	screenOptionRe    = regexp.MustCompile(`^\s*(❯\s*)?(?:(\d+)\.\s+)?(\S.*?)\s*$`)
)

func screenInputReady(screen string) bool {
	return strings.Contains(screen, "shift+tab") || strings.Contains(screen, "shortcuts")
}

// screenPermissionMode reads the status line under the input box; the
// CLI shows nothing there for the default (manual) mode.
func screenPermissionMode(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, " \n"), "\n")
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	tail := strings.ToLower(strings.Join(lines, " "))
	switch {
	case strings.Contains(tail, "auto mode"):
		return "auto"
	case strings.Contains(tail, "accept edits"):
		return "acceptEdits"
	case strings.Contains(tail, "plan mode"):
		return "plan"
	case strings.Contains(tail, "bypass"):
		return "bypassPermissions"
	}
	return "manual"
}

// parseScreenPrompt finds a menu: the block of lines right above a
// "Esc to cancel" style footer, holding the ❯ cursor. Numbered menus keep
// only numbered lines, since their options may carry description lines.
func parseScreenPrompt(screen string) *ScreenPrompt {
	lines := strings.Split(strings.TrimRight(screen, " \n"), "\n")
	// The status line only shows under the input box. A footer above it is
	// leftover text from an earlier frame, not a menu.
	last := lines
	if len(last) > 3 {
		last = last[len(last)-3:]
	}
	if screenInputReady(strings.Join(last, "\n")) {
		return nil
	}
	footer := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if screenFooterRe.MatchString(lines[i]) {
			footer = i
			break
		}
	}
	if footer < 0 {
		return nil
	}
	end := footer
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	start := end
	numbered := false
	for start > 0 && end-start < 16 {
		l := strings.TrimSpace(lines[start-1])
		if l == "" {
			if numbered {
				start--
				continue
			}
			break
		}
		if m := screenOptionRe.FindStringSubmatch(lines[start-1]); m != nil && m[2] != "" {
			numbered = true
		} else if numbered && !strings.HasPrefix(lines[start-1], " ") {
			break
		}
		start--
	}
	p := &ScreenPrompt{Selected: -1}
	for _, l := range lines[start:end] {
		m := screenOptionRe.FindStringSubmatch(l)
		if m == nil || (numbered && m[2] == "") {
			continue
		}
		if m[1] != "" {
			p.Selected = len(p.Options)
		}
		p.Options = append(p.Options, m[3])
	}
	if p.Selected < 0 || len(p.Options) < 2 || len(p.Options) > 9 {
		return nil
	}
	flat := strings.ToLower(strings.Join(p.Options, " "))
	p.Trust = strings.Contains(flat, "trust this folder")
	// Unnumbered blocks are only trusted for the one known dialog; anything
	// else above a footer is ordinary screen text.
	if !numbered && !p.Trust {
		return nil
	}
	var title []string
	for i := start - 1; i >= 0 && len(title) < 3; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			if len(title) > 0 {
				break
			}
			continue
		}
		if screenRuleRe.MatchString(l) {
			break
		}
		title = append([]string{l}, title...)
	}
	p.Title = strings.Join(title, " ")
	return p
}

// conversationTranscriptPath locates the JSONL the session is writing.
// The workspace dir is matched exactly: the fuzzy matcher used elsewhere
// can land on a sibling whose name merely contains this cwd's parts.
func conversationTranscriptPath(cfg ClaudeHistoryConfig, workDir, storedID string) string {
	if storedID == "" {
		return ""
	}
	for _, root := range resolveClaudeRoots(cfg) {
		id := resolveClaudeResumeID(filepath.Dir(root), workDir, storedID)
		if id == "" {
			continue
		}
		p := filepath.Join(root, claudeWorkspaceName(workDir), id+".jsonl")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

type lastMsgCacheEntry struct {
	size  int64
	mtime time.Time
	at    time.Time
	ok    bool
}

var (
	lastMsgCacheMu sync.Mutex
	lastMsgCache   = map[string]lastMsgCacheEntry{}
)

// lastMessageTime returns the timestamp of the transcript's latest
// displayable message, scanning only the tail of the file.
func lastMessageTime(path string) (time.Time, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	lastMsgCacheMu.Lock()
	c, hit := lastMsgCache[path]
	lastMsgCacheMu.Unlock()
	if hit && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		return c.at, c.ok
	}
	entry := lastMsgCacheEntry{size: st.Size(), mtime: st.ModTime()}
	if f, err := os.Open(path); err == nil {
		if off := st.Size() - oldChatProbeBytes; off > 0 {
			_, _ = f.Seek(off, io.SeekStart)
		}
		tail, _ := io.ReadAll(f)
		f.Close()
		lines := bytes.Split(tail, []byte{'\n'})
		for i := len(lines) - 1; i >= 0; i-- {
			if msg, ok := parseConversationLine(lines[i]); ok && !msg.Ts.IsZero() {
				entry.at, entry.ok = msg.Ts, true
				break
			}
		}
	}
	lastMsgCacheMu.Lock()
	lastMsgCache[path] = entry
	lastMsgCacheMu.Unlock()
	return entry.at, entry.ok
}

func conversationUserText(msg ConversationMessage) string {
	var b strings.Builder
	for _, bl := range msg.Blocks {
		if bl.Type == "text" {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// convCache keeps each transcript's parsed messages between polls. The
// chat polls every ~1s while Claude works and transcripts reach tens of
// MB, so re-parsing the whole file each time cost seconds and CPU; now
// only the bytes appended since the last read are parsed.
type convCache struct {
	mu     sync.Mutex
	size   int64
	mtime  time.Time
	off    int64 // bytes consumed: always ends on a complete line
	msgs   []ConversationMessage
	queued map[string]int
}

var (
	convCachesMu sync.Mutex
	convCaches   = map[string]*convCache{}
)

func readConversation(path string) ([]ConversationMessage, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	convCachesMu.Lock()
	c := convCaches[path]
	if c == nil {
		c = &convCache{}
		convCaches[path] = c
	}
	convCachesMu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if st.Size() == c.size && st.ModTime().Equal(c.mtime) && c.msgs != nil {
		return c.snapshot(), nil
	}
	if st.Size() < c.off {
		// Rewritten or truncated: start over.
		c.off, c.msgs, c.queued = 0, nil, nil
	}
	if c.queued == nil {
		c.queued = map[string]int{}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(c.off, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// A line without its newline is still being written; leave
			// it for the next read.
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		c.off += int64(len(line))
		c.add(line)
	}
	if c.msgs == nil {
		c.msgs = []ConversationMessage{}
	}
	c.size, c.mtime = st.Size(), st.ModTime()
	return c.snapshot(), nil
}

func (c *convCache) add(line []byte) {
	msg, ok := parseConversationLine(line)
	if !ok {
		return
	}
	if msg.Role == "user" {
		// Prompts already shown from a queued_command attachment; if
		// Claude Code later also writes the same text as a plain user
		// line, skip it.
		text := strings.TrimSpace(conversationUserText(msg))
		if bytes.Contains(line, []byte(`"queued_command"`)) {
			c.queued[text]++
		} else if text != "" && c.queued[text] > 0 {
			c.queued[text]--
			return
		}
	}
	c.msgs = append(c.msgs, msg)
	// Keep memory bounded; only the tail is ever shown.
	if len(c.msgs) > 2*conversationMaxMessages {
		c.msgs = append([]ConversationMessage(nil), c.msgs[len(c.msgs)-conversationMaxMessages:]...)
	}
}

func (c *convCache) snapshot() []ConversationMessage {
	msgs := c.msgs
	if len(msgs) > conversationMaxMessages {
		msgs = msgs[len(msgs)-conversationMaxMessages:]
	}
	return append([]ConversationMessage(nil), msgs...)
}

func parseConversationLine(line []byte) (ConversationMessage, bool) {
	var e conversationLine
	if json.Unmarshal(line, &e) != nil {
		return ConversationMessage{}, false
	}
	if e.Type == "attachment" && e.Attachment != nil && e.Attachment.Type == "queued_command" &&
		!e.IsSidechain && strings.TrimSpace(e.Attachment.Prompt) != "" {
		msg := ConversationMessage{ID: e.UUID, Role: "user", Ts: e.Time}
		msg.Blocks = appendConversationText(msg.Blocks, "user", e.Attachment.Prompt)
		return msg, len(msg.Blocks) > 0
	}
	if e.Message == nil {
		return ConversationMessage{}, false
	}
	if (e.Type != "user" && e.Type != "assistant") || e.IsMeta || e.IsSidechain || e.IsCompact {
		return ConversationMessage{}, false
	}
	msg := ConversationMessage{ID: e.UUID, Role: e.Type, Ts: e.Time}
	raw := e.Message.Content
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			msg.Blocks = appendConversationText(msg.Blocks, e.Type, s)
		}
	} else {
		blocks, err := parseClaudeContentBlocks(raw)
		if err != nil {
			return ConversationMessage{}, false
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				msg.Blocks = appendConversationText(msg.Blocks, e.Type, b.Text)
			case "tool_use":
				block := ConversationBlock{
					Type: "tool_use", Name: b.Name, Summary: formatToolUse(b), ToolID: conversationToolID(line, b),
				}
				if in := b.Input; in != nil {
					block.Desc = in.Description
					for _, v := range []string{in.FilePath, in.Path, in.Pattern, in.Query, in.URL} {
						if v != "" {
							block.Target = v
							break
						}
					}
				}
				if b.Name == "AskUserQuestion" {
					block.Ask = askUserQuestions(line, block.ToolID)
				}
				msg.Blocks = append(msg.Blocks, block)
			case "tool_result":
				msg.Blocks = append(msg.Blocks, ConversationBlock{
					Type: "tool_result", Text: truncateRunes(b.ToolResultText(), conversationToolCap),
					ToolID: b.ToolUseID, IsError: b.IsError,
				})
			}
		}
	}
	return msg, len(msg.Blocks) > 0
}

// askUserQuestions returns the raw questions array of the AskUserQuestion
// call with the given id.
func askUserQuestions(line []byte, toolID string) json.RawMessage {
	var doc struct {
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Input struct {
					Questions json.RawMessage `json:"questions"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &doc) != nil {
		return nil
	}
	for _, c := range doc.Message.Content {
		if c.Type == "tool_use" && (toolID == "" || c.ID == toolID) && len(c.Input.Questions) > 0 {
			return c.Input.Questions
		}
	}
	return nil
}

// conversationToolID pulls the tool_use id, which claudeContentBlock does
// not decode (it only keeps the tool_use_id of results).
func conversationToolID(line []byte, b claudeContentBlock) string {
	var doc struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &doc) != nil {
		return ""
	}
	for _, c := range doc.Message.Content {
		if c.Type == "tool_use" && c.Name == b.Name {
			return c.ID
		}
	}
	return ""
}

func appendConversationText(blocks []ConversationBlock, role, text string) []ConversationBlock {
	t := strings.TrimSpace(text)
	if t == "" {
		return blocks
	}
	if role == "user" {
		for _, p := range []string{"<command-", "<local-command", "<system-reminder", "<bash-", "<task-notification", "Caveat:"} {
			if strings.HasPrefix(t, p) {
				return blocks
			}
		}
	}
	return append(blocks, ConversationBlock{Type: "text", Text: t})
}

// limitIsFresh confirms a banner seen on screen is a new limit. The TUI
// re-renders old transcript text on resume and on resize, so the banner
// alone proves nothing; the transcript's own entry carries the real time.
func limitIsFresh(cfg ClaudeHistoryConfig, workDir, claudeID string, now time.Time) bool {
	path := conversationTranscriptPath(cfg, workDir, claudeID)
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	if off := st.Size() - oldChatProbeBytes; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return false
		}
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	lines := bytes.Split(tail, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		if !bytes.Contains(lines[i], []byte(`"error":"rate_limit"`)) {
			continue
		}
		var e struct {
			Time time.Time `json:"timestamp"`
		}
		if json.Unmarshal(lines[i], &e) != nil {
			continue
		}
		return now.Sub(e.Time) < limitFreshSpan
	}
	return false
}

const limitFreshSpan = 3 * time.Minute

// ── Earlier Claude conversations ────────────────────────────────────

// OldChat is a Claude transcript on this host that can be opened as a chat.
type OldChat struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Cwd     string    `json:"cwd"`
	Updated time.Time `json:"updated"`
	// InUse: written to moments ago, so another client probably has it
	// open. It can be read here, but not continued until it goes quiet.
	InUse bool `json:"in_use,omitempty"`
}

type oldChatCacheEntry struct {
	size  int64
	mtime time.Time
	chat  OldChat
	ok    bool
}

const (
	oldChatProbeBytes = 512 << 10
	// A transcript touched this recently may belong to a live CLI or
	// desktop session; resuming it here would race that writer.
	oldChatQuietSpan = 10 * time.Minute
)

var (
	oldChatCacheMu sync.Mutex
	oldChatCache   = map[string]oldChatCacheEntry{}
	oldChatIDRe    = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
)

// OldChats lists transcripts newest first, skipping sub-agent files and
// anything whose folder no longer exists.
func (m *Manager) OldChats(_ context.Context) []OldChat {
	seen := map[string]bool{}
	out := []OldChat{}
	for _, root := range resolveClaudeRoots(m.claudeHistoryCfg) {
		canon, err := filepath.EvalSymlinks(root)
		if err != nil || seen[canon] {
			continue
		}
		seen[canon] = true
		files, _ := filepath.Glob(filepath.Join(canon, "*", "*.jsonl"))
		for _, p := range files {
			id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
			if !oldChatIDRe.MatchString(id) {
				continue
			}
			st, err := os.Lstat(p)
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			if chat, ok := summarizeOldChat(p, id, st); ok {
				chat.InUse = time.Since(st.ModTime()) < oldChatQuietSpan
				out = append(out, chat)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

func summarizeOldChat(path, id string, st os.FileInfo) (OldChat, bool) {
	oldChatCacheMu.Lock()
	c, hit := oldChatCache[path]
	oldChatCacheMu.Unlock()
	if hit && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		return c.chat, c.ok
	}
	chat, ok := readOldChat(path, id, st)
	oldChatCacheMu.Lock()
	oldChatCache[path] = oldChatCacheEntry{size: st.Size(), mtime: st.ModTime(), chat: chat, ok: ok}
	oldChatCacheMu.Unlock()
	return chat, ok
}

func readOldChat(path, id string, st os.FileInfo) (OldChat, bool) {
	f, err := os.Open(path)
	if err != nil {
		return OldChat{}, false
	}
	defer f.Close()
	chat := OldChat{ID: id, Updated: st.ModTime()}

	var first string
	r := bufio.NewReaderSize(io.LimitReader(f, 4*oldChatProbeBytes), 1<<20)
	for first == "" {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, []byte(`"type":"user"`)) {
			var e struct {
				Cwd string `json:"cwd"`
			}
			if msg, ok := parseConversationLine(line); ok && msg.Role == "user" {
				for _, b := range msg.Blocks {
					if b.Type == "text" {
						first = b.Text
						break
					}
				}
				if first != "" && json.Unmarshal(line, &e) == nil {
					chat.Cwd = e.Cwd
				}
			}
		}
		if err != nil {
			break
		}
	}
	if first == "" || chat.Cwd == "" {
		return OldChat{}, false
	}
	if d, err := os.Stat(chat.Cwd); err != nil || !d.IsDir() {
		return OldChat{}, false
	}
	// Throwaway runs from temp folders aren't conversations worth listing.
	if strings.HasPrefix(chat.Cwd, "/private/tmp/") || strings.HasPrefix(chat.Cwd, "/tmp/") {
		return OldChat{}, false
	}
	chat.Title = oldChatTitle(f, st.Size())
	if chat.Title == "" {
		chat.Title = truncateRunes(strings.Join(strings.Fields(first), " "), 70)
	}
	return chat, true
}

// oldChatTitle returns the newest custom-title in the file's tail.
func oldChatTitle(f *os.File, size int64) string {
	start := size - oldChatProbeBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	lines := bytes.Split(tail, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		if !bytes.Contains(lines[i], []byte(`"custom-title"`)) {
			continue
		}
		var e struct {
			Title string `json:"customTitle"`
		}
		if json.Unmarshal(lines[i], &e) == nil && e.Title != "" {
			return e.Title
		}
	}
	return ""
}

// ImportOldChat registers a transcript as a stopped session. Nothing is
// spawned: the chat opens read-only from the transcript and the first
// message resumes it through the normal Start path (--resume).
func (m *Manager) ImportOldChat(ctx context.Context, claudeID, accountID string) (Session, error) {
	all, err := m.List(ctx)
	if err != nil {
		return Session{}, err
	}
	for _, s := range all {
		if s.ClaudeSessionID == claudeID {
			return s, nil
		}
	}
	for _, c := range m.OldChats(ctx) {
		if c.ID != claudeID {
			continue
		}
		sess := Session{
			ID: newID(), Name: c.Title, ProviderID: "claude", Cwd: c.Cwd, Args: []string{},
			State: StateStopped, ClaudeAccountID: accountID, ClaudeSessionID: claudeID,
			Origin: OriginOperator, StartedAt: time.Now().UTC(),
		}
		if err := m.store.Insert(ctx, sess); err != nil {
			return Session{}, err
		}
		return m.Get(ctx, sess.ID)
	}
	return Session{}, ErrNotFound
}

// ── Per-account usage ───────────────────────────────────────────────

// AccountUsage sums what this host's opendray sessions consumed. Cache
// reads are left out: they dwarf everything and barely count to limits.
type AccountUsage struct {
	Today int64 `json:"today"`
	Week  int64 `json:"week"`
}

type usageCacheEntry struct {
	size  int64
	mtime time.Time
	days  map[string]int64
}

var (
	usageCacheMu sync.Mutex
	usageCache   = map[string]usageCacheEntry{}
)

// ChatUsage attributes each session's tokens to its current account.
// Only entrypoint=cli lines count, so desktop-era turns of an imported
// conversation are not charged to an opendray account.
func (m *Manager) ChatUsage(ctx context.Context) (map[string]AccountUsage, error) {
	all, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	week := map[string]bool{}
	for i := 0; i < 7; i++ {
		week[now.AddDate(0, 0, -i).Format("2006-01-02")] = true
	}
	out := map[string]AccountUsage{}
	seen := map[string]bool{}
	for _, s := range all {
		if s.ProviderID != "claude" || s.ClaudeAccountID == "" {
			continue
		}
		p := conversationTranscriptPath(m.claudeHistoryCfg, s.EffectiveWorkDir(), s.ClaudeSessionID)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		u := out[s.ClaudeAccountID]
		for day, n := range transcriptUsage(p) {
			if day == today {
				u.Today += n
			}
			if week[day] {
				u.Week += n
			}
		}
		out[s.ClaudeAccountID] = u
	}
	return out, nil
}

func transcriptUsage(path string) map[string]int64 {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	usageCacheMu.Lock()
	c, hit := usageCache[path]
	usageCacheMu.Unlock()
	if hit && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		return c.days
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	type turn struct {
		day string
		n   int64
	}
	turns := map[string]turn{}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"usage"`)) && bytes.Contains(line, []byte(`"type":"assistant"`)) {
			var e struct {
				Entrypoint string    `json:"entrypoint"`
				Time       time.Time `json:"timestamp"`
				Message    struct {
					ID    string `json:"id"`
					Usage struct {
						Input  int64 `json:"input_tokens"`
						Output int64 `json:"output_tokens"`
						Cache  int64 `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &e) == nil && e.Entrypoint == "cli" && e.Message.ID != "" {
				// One API message spans several lines; the last is final.
				turns[e.Message.ID] = turn{
					day: e.Time.Local().Format("2006-01-02"),
					n:   e.Message.Usage.Input + e.Message.Usage.Output + e.Message.Usage.Cache,
				}
			}
		}
		if err != nil {
			break
		}
	}
	days := map[string]int64{}
	for _, t := range turns {
		days[t.day] += t.n
	}
	usageCacheMu.Lock()
	usageCache[path] = usageCacheEntry{size: st.Size(), mtime: st.ModTime(), days: days}
	usageCacheMu.Unlock()
	return days
}

// ── Chat metadata (titles, topics) ──────────────────────────────────

// ChatMeta is the whole metadata document. Single-operator data, so a
// JSON file next to the config is enough and needs no schema migration.
type ChatMeta struct {
	Topics []string                 `json:"topics"`
	Chats  map[string]ChatMetaEntry `json:"chats"`
}

// ChatMetaEntry overrides one session's display.
type ChatMetaEntry struct {
	Title string `json:"title,omitempty"`
	Topic string `json:"topic,omitempty"`
}

var chatMetaMu sync.Mutex

func chatMetaPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".opendray", "chat-meta.json"), nil
}

func loadChatMeta() (ChatMeta, error) {
	meta := ChatMeta{Topics: []string{}, Chats: map[string]ChatMetaEntry{}}
	p, err := chatMetaPath()
	if err != nil {
		return meta, err
	}
	body, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return meta, nil
	}
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return meta, err
	}
	if meta.Topics == nil {
		meta.Topics = []string{}
	}
	if meta.Chats == nil {
		meta.Chats = map[string]ChatMetaEntry{}
	}
	return meta, nil
}

func saveChatMeta(meta ChatMeta) error {
	p, err := chatMetaPath()
	if err != nil {
		return err
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ── HTTP ────────────────────────────────────────────────────────────

type conversationService interface {
	Conversation(ctx context.Context, id string) (ConversationResponse, error)
	OldChats(ctx context.Context) []OldChat
	ChatUsage(ctx context.Context) (map[string]AccountUsage, error)
	ImportOldChat(ctx context.Context, claudeID, accountID string) (Session, error)
	ChatActivity(ctx context.Context) (ChatActivityResponse, error)
}

// ChatActivityResponse feeds the chat sidebar: when each chat last had a
// message (for ordering) and which chats Claude is working in right now.
type ChatActivityResponse struct {
	Sessions map[string]time.Time `json:"sessions"`
	Working  []string             `json:"working"`
}

func (h *Handlers) chatActivity(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.svc.(conversationService)
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("chat activity not available"))
		return
	}
	res, err := svc.ChatActivity(r.Context())
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ChatActivity maps each Claude session to the time of its latest real
// message from either side, which is what the chat sidebar sorts by.
// The file's mtime is no good: a resume after a restart writes to every
// transcript and would float every chat to the top.
func (m *Manager) ChatActivity(ctx context.Context) (ChatActivityResponse, error) {
	list, err := m.List(ctx)
	if err != nil {
		return ChatActivityResponse{}, err
	}
	out := ChatActivityResponse{Sessions: make(map[string]time.Time, len(list)), Working: []string{}}
	for _, sess := range list {
		if sess.ProviderID != "claude" || sess.ClaudeSessionID == "" {
			continue
		}
		path := conversationTranscriptPath(m.claudeHistoryCfg, sess.EffectiveWorkDir(), sess.ClaudeSessionID)
		if path != "" {
			if t, ok := lastMessageTime(path); ok {
				out.Sessions[sess.ID] = t.UTC()
			}
		}
		if sess.State.IsTerminal() {
			continue
		}
		// Working = the TUI shows its spinner, or the transcript is being
		// written this very moment. An open but idle chat is not working.
		busy := screenBusy(m.RecentScreen(sess.ID))
		if !busy && path != "" {
			if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < conversationActiveSpan {
				busy = true
			}
		}
		if busy {
			out.Working = append(out.Working, sess.ID)
		}
	}
	return out, nil
}

func (h *Handlers) conversation(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.svc.(conversationService)
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("conversation not available"))
		return
	}
	res, err := svc.Conversation(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handlers) chatUsage(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.svc.(conversationService)
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("usage not available"))
		return
	}
	res, err := svc.ChatUsage(r.Context())
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}

func (h *Handlers) oldChats(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.svc.(conversationService)
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("old chats not available"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": svc.OldChats(r.Context())})
}

func (h *Handlers) importOldChat(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.svc.(conversationService)
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("old chats not available"))
		return
	}
	var req struct {
		ClaudeAccountID string `json:"claude_account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sess, err := svc.ImportOldChat(r.Context(), chi.URLParam(r, "claudeID"), req.ClaudeAccountID)
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (h *Handlers) getChatMeta(w http.ResponseWriter, _ *http.Request) {
	chatMetaMu.Lock()
	defer chatMetaMu.Unlock()
	meta, err := loadChatMeta()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (h *Handlers) putChatMeta(w http.ResponseWriter, r *http.Request) {
	var meta ChatMeta
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&meta); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	chatMetaMu.Lock()
	defer chatMetaMu.Unlock()
	if err := saveChatMeta(meta); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}
