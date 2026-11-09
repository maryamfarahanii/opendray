package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanForRateLimitBanner_StandardForm(t *testing.T) {
	// The exact form observed in the operator's screenshot.
	buf := []byte("You've hit your session limit · resets 10:20am (UTC)\n")
	now := mustTime(t, "2006-01-02T08:00:00Z")
	reset, ok := ScanForRateLimitBanner(buf, now)
	if !ok {
		t.Fatal("expected banner to be detected")
	}
	want := mustTime(t, "2006-01-02T10:20:00Z")
	if !reset.Equal(want) {
		t.Errorf("reset = %v, want %v", reset, want)
	}
}

func TestScanForRateLimitBanner_24HourForm(t *testing.T) {
	buf := []byte("You've hit your session limit · resets 22:30 (UTC)\n")
	now := mustTime(t, "2006-01-02T08:00:00Z")
	reset, ok := ScanForRateLimitBanner(buf, now)
	if !ok {
		t.Fatal("expected banner to be detected")
	}
	want := mustTime(t, "2006-01-02T22:30:00Z")
	if !reset.Equal(want) {
		t.Errorf("reset = %v, want %v", reset, want)
	}
}

func TestScanForRateLimitBanner_RollsToTomorrowWhenPast(t *testing.T) {
	// Reset time HH:MM is already past for today → reset is tomorrow.
	buf := []byte("You've hit your session limit · resets 09:00 (UTC)\n")
	now := mustTime(t, "2006-01-02T15:00:00Z")
	reset, ok := ScanForRateLimitBanner(buf, now)
	if !ok {
		t.Fatal("expected banner to be detected")
	}
	want := mustTime(t, "2006-01-03T09:00:00Z")
	if !reset.Equal(want) {
		t.Errorf("reset = %v, want %v", reset, want)
	}
}

func TestScanForRateLimitBanner_HyphenSeparatorFallback(t *testing.T) {
	// Future-proofing: if the CLI swaps the · for a -, we still match.
	buf := []byte("You've hit your session limit - resets 11:00am (UTC)\n")
	now := mustTime(t, "2006-01-02T08:00:00Z")
	_, ok := ScanForRateLimitBanner(buf, now)
	if !ok {
		t.Error("expected hyphen-separator banner to match")
	}
}

func TestScanForRateLimitBanner_NoMatchOnUnrelatedOutput(t *testing.T) {
	tests := []string{
		"Some output that mentions session and limit but not the banner",
		"You hit your session limit yesterday but this is a recap",
		"You've hit your session limit · resets soon",    // no time
		"You've hit your session limit · resets 10:20am", // no (UTC)
	}
	now := mustTime(t, "2006-01-02T08:00:00Z")
	for _, s := range tests {
		t.Run(s[:min(40, len(s))], func(t *testing.T) {
			if _, ok := ScanForRateLimitBanner([]byte(s), now); ok {
				t.Errorf("expected NO match on: %q", s)
			}
		})
	}
}

func TestScanForRateLimitBanner_BannerEmbeddedInLargerOutput(t *testing.T) {
	// Realistic: the banner appears in the middle of a stream of TUI
	// redraws and ANSI escape codes.
	buf := []byte(
		"\x1b[2J\x1b[H> some prompt\n" +
			"Response chunk here...\n" +
			"\x1b[31mYou've hit your session limit · resets 14:00 (UTC)\x1b[0m\n" +
			"more output trailing...\n",
	)
	now := mustTime(t, "2006-01-02T08:00:00Z")
	reset, ok := ScanForRateLimitBanner(buf, now)
	if !ok {
		t.Fatal("expected banner to be detected even when surrounded by ANSI")
	}
	want := mustTime(t, "2006-01-02T14:00:00Z")
	if !reset.Equal(want) {
		t.Errorf("reset = %v, want %v", reset, want)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestScanForRateLimitBanner_FiveHourTehranWithCursorMoves(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 11, 5, 20, 0, 0, 0, loc)
	raw := []byte("\x1b[1m\x1b[38;2;1;2;3mYou've\x1b[1Chit\x1b[1Cyour\x1b[1C5-hour\x1b[1Climit\x1b[1C·\x1b[1Cresets\x1b[1C11:10pm\x1b[1C(Asia/Tehran)\x1b[0m")
	got, ok := ScanForRateLimitBanner(raw, now)
	if !ok {
		t.Fatal("banner not matched")
	}
	want := time.Date(2026, 11, 5, 23, 10, 0, 0, loc).UTC()
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestScanForRateLimitBanner_NoSpacesAtAll(t *testing.T) {
	if _, ok := ScanForRateLimitBanner([]byte("You'vehityour5-hourlimit·resets11:10pm(UTC)"), time.Now()); !ok {
		t.Fatal("banner not matched")
	}
}

func TestScanForRateLimitBanner_WeeklyWithDate(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	got, ok := ScanForRateLimitBanner([]byte("You've hit your weekly limit · resets Nov 7, 3am (UTC)"), now)
	if !ok {
		t.Fatal("banner not matched")
	}
	if want := time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseScreenPrompt_Trust(t *testing.T) {
	screen := "Quick safety check\nIs this a project you trust?\n\n  ❯ No, exit\n    Yes, I trust this folder\n\nEnter to confirm · Esc to cancel\n"
	p := parseScreenPrompt(screen)
	if p == nil || !p.Trust || p.Selected != 0 || len(p.Options) != 2 || p.Options[1] != "Yes, I trust this folder" {
		t.Fatalf("got %+v", p)
	}
}

func TestParseScreenPrompt_Numbered(t *testing.T) {
	screen := "Bash command\n  rm -rf build\nDo you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again\n  3. No\n\nEsc to cancel · Tab to amend\n"
	p := parseScreenPrompt(screen)
	if p == nil || p.Trust || p.Selected != 0 || len(p.Options) != 3 || p.Options[2] != "No" || p.Title == "" {
		t.Fatalf("got %+v", p)
	}
}

func TestParseScreenPrompt_NoneOnInputBox(t *testing.T) {
	if p := parseScreenPrompt("hello\n❯ \n  ►► auto mode on (shift+tab to cycle)\n"); p != nil {
		t.Fatalf("got %+v", p)
	}
}

func TestScreenPermissionMode(t *testing.T) {
	cases := map[string]string{
		"x\n  ►► auto mode on (shift+tab to cycle)": "auto",
		"x\n  ⏵⏵ accept edits on (shift+tab)":       "acceptEdits",
		"x\n  ⏸ plan mode on (shift+tab to cycle)":  "plan",
		"x\n  ? for shortcuts":                      "manual",
	}
	for in, want := range cases {
		if got := screenPermissionMode(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

func TestLimitIsFresh(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "projects")
	work := "/tmp/work"
	dir := filepath.Join(root, claudeWorkspaceName(work))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-1111-1111-111111111111"
	cfg := ClaudeHistoryConfig{HistoryRoots: []string{root}}
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	write := func(at time.Time) {
		line := fmt.Sprintf(`{"type":"assistant","error":"rate_limit","timestamp":%q}`+"\n", at.Format(time.RFC3339))
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(now.Add(-time.Minute))
	if !limitIsFresh(cfg, work, id, now) {
		t.Fatal("a limit from a minute ago must count")
	}
	write(now.Add(-18 * time.Hour))
	if limitIsFresh(cfg, work, id, now) {
		t.Fatal("yesterday's limit re-rendered on resume must not count")
	}
	if limitIsFresh(cfg, work, "22222222-2222-2222-2222-222222222222", now) {
		t.Fatal("no transcript must not count")
	}
}

func TestParseScreenPrompt_IgnoresLeftoverFooter(t *testing.T) {
	screen := "These will apply without asking.\n❯ leftover\nsome text\nEnter to confirm · Esc to cancel\n───────\n❯ \n───────\n  ►► auto mode on (shift+tab to cycle) · ← for agents\n"
	if p := parseScreenPrompt(screen); p != nil {
		t.Fatalf("got %+v", p)
	}
	if p := parseScreenPrompt("garbled\n❯ one\ntwo\nthree\n\nEsc to cancel\n"); p != nil {
		t.Fatalf("unnumbered non-trust block: got %+v", p)
	}
}

// A banner arriving inside the cooldown must queue exactly one trailing
// scan, or a limit printed as the TUI's last output is never seen.
func TestAppendRateLimitWindowQueuesTrailingScan(t *testing.T) {
	rs := &runningSession{}
	t0 := time.Now()
	if scan, _ := rs.appendRateLimitWindow([]byte("a"), t0); !scan {
		t.Fatal("first chunk should scan")
	}
	scan, wait := rs.appendRateLimitWindow([]byte("limit banner"), t0.Add(time.Second))
	if scan || wait != rateLimitScanCooldown-time.Second {
		t.Fatalf("want trailing scan in %v, got scan=%v wait=%v", rateLimitScanCooldown-time.Second, scan, wait)
	}
	if _, wait := rs.appendRateLimitWindow([]byte("b"), t0.Add(2*time.Second)); wait != 0 {
		t.Fatal("only one trailing scan may be queued")
	}
	if !rs.startTrailingRateLimitScan(t0.Add(rateLimitScanCooldown)) {
		t.Fatal("trailing scan should run after the cooldown")
	}
	if _, wait := rs.appendRateLimitWindow([]byte("c"), t0.Add(rateLimitScanCooldown+time.Second)); wait == 0 {
		t.Fatal("a new trailing scan should be queueable again")
	}
}

// A message sent mid-turn is stored as a queued_command attachment; it
// must show as a user message, or the chat waits on it forever.
func TestReadConversationQueuedCommand(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/t.jsonl"
	lines := `{"type":"user","uuid":"u1","timestamp":"2026-11-06T08:42:48Z","message":{"role":"user","content":"اول"}}
{"type":"attachment","uuid":"q1","timestamp":"2026-11-06T08:44:21Z","attachment":{"type":"queued_command","prompt":"دوم"}}
{"type":"user","uuid":"u2","timestamp":"2026-11-06T08:44:22Z","message":{"role":"user","content":"دوم"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-11-06T08:56:16Z","message":{"role":"assistant","content":[{"type":"text","text":"تمام"}]}}
`
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, err := readConversation(p)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range msgs {
		got = append(got, m.Role+":"+conversationUserText(m))
	}
	want := []string{"user:اول", "user:دوم", "assistant:تمام"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLastMessageTimeIgnoresNonMessages(t *testing.T) {
	p := t.TempDir() + "/t.jsonl"
	lines := `{"type":"user","uuid":"u1","timestamp":"2026-11-06T08:42:48Z","message":{"role":"user","content":"سلام"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-11-06T08:56:16Z","message":{"role":"assistant","content":[{"type":"text","text":"تمام"}]}}
{"type":"system","uuid":"s1","timestamp":"2026-11-06T09:07:54Z","subtype":"turn_duration"}
`
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := lastMessageTime(p)
	if !ok || !got.Equal(time.Date(2026, 11, 6, 8, 56, 16, 0, time.UTC)) {
		t.Fatalf("got %v %v", got, ok)
	}
}

func TestParseConversationLineAskUserQuestion(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"a1","timestamp":"2026-11-06T10:45:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"AskUserQuestion","input":{"questions":[{"question":"ظرفیت؟","header":"ظرفیت","multiSelect":false,"options":[{"label":"۲۰ نفر","description":"کوچک"},{"label":"۵۰ نفر","description":"بزرگ"}]}]}}]}}`)
	msg, ok := parseConversationLine(line)
	if !ok || len(msg.Blocks) != 1 {
		t.Fatalf("parse failed: %+v", msg)
	}
	b := msg.Blocks[0]
	if b.ToolID != "toolu_1" || !strings.Contains(string(b.Ask), "۵۰ نفر") {
		t.Fatalf("ask not carried: id=%q ask=%s", b.ToolID, b.Ask)
	}
}

// Appending to a transcript must show the new message without losing the
// old ones, and a half-written last line must wait for its newline.
func TestReadConversationIncremental(t *testing.T) {
	p := t.TempDir() + "/t.jsonl"
	first := `{"type":"user","uuid":"u1","timestamp":"2026-11-06T08:00:00Z","message":{"role":"user","content":"یک"}}` + "\n"
	if err := os.WriteFile(p, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := readConversation(p); len(msgs) != 1 {
		t.Fatalf("want 1, got %d", len(msgs))
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	half := `{"type":"assistant","uuid":"a1","timestamp":"2026-11-06T08:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"دو"}]}}`
	_, _ = f.WriteString(half[:40])
	_ = f.Sync()
	if msgs, _ := readConversation(p); len(msgs) != 1 {
		t.Fatalf("half line must wait, got %d", len(msgs))
	}
	_, _ = f.WriteString(half[40:] + "\n")
	f.Close()
	msgs, _ := readConversation(p)
	if len(msgs) != 2 || conversationUserText(msgs[1]) != "دو" {
		t.Fatalf("got %+v", msgs)
	}
}
