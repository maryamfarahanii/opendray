package session

import (
	"regexp"
	"strings"
	"time"
)

// rateLimitBannerRe matches the Claude CLI's limit banner. Observed forms:
//
//	You've hit your session limit · resets 10:20am (UTC)
//	You've hit your 5-hour limit · resets 11:10pm (Asia/Tehran)
//	You've hit your weekly limit · resets Nov 7, 3am (Asia/Tehran)
//
// The kind of limit and the zone both vary by plan and host, so they are
// matched loosely. \s* rather than \s+ because the TUI positions words
// with cursor moves, which can leave no space at all once stripped.
var rateLimitBannerRe = regexp.MustCompile(
	`(?i)you(?:'?ve|\s*have)\s*hit\s*your\s*[^·|\r\n]{0,40}?limit\s*[·\-|–—]+\s*resets?\s*([^()\r\n]{1,32}?)\s*\(\s*([A-Za-z0-9_/+\-]+)\s*\)`,
)

var (
	ansiCursorRightRe = regexp.MustCompile(`\x1b\[\d*C`)
	ansiSequenceRe    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[@-_]`)
	resetClockRe      = regexp.MustCompile(`(?i)(\d{1,2}(?::\d{2})?\s*(?:am|pm)?)\s*$`)
)

// fallbackResetSpan is used when a real banner's reset text can't be parsed.
const fallbackResetSpan = 5 * time.Hour

// stripTerminalCodes removes control sequences from raw PTY bytes, turning
// cursor-forward moves back into the spaces they stand for.
func stripTerminalCodes(b []byte) []byte {
	return ansiSequenceRe.ReplaceAll(ansiCursorRightRe.ReplaceAll(b, []byte(" ")), nil)
}

// ScanForRateLimitBanner inspects raw PTY output for the newest limit
// banner. Returns the reset instant in UTC, or (zero time, false). Caller
// passes 'now' so tests are deterministic.
func ScanForRateLimitBanner(window []byte, now time.Time) (time.Time, bool) {
	all := rateLimitBannerRe.FindAllSubmatch(stripTerminalCodes(window), -1)
	if len(all) == 0 {
		return time.Time{}, false
	}
	m := all[len(all)-1]
	loc, err := time.LoadLocation(string(m[2]))
	if err != nil {
		loc = time.UTC
	}
	return resolveReset(string(m[1]), now, loc), true
}

// resolveReset reads "11:10pm" or "Nov 7, 3am" in loc. The banner itself
// is already proven real here, so unreadable text degrades to a fixed span.
func resolveReset(text string, now time.Time, loc *time.Location) time.Time {
	text = strings.TrimSpace(text)
	clock := resetClockRe.FindStringSubmatch(text)
	if clock == nil {
		return now.Add(fallbackResetSpan).UTC()
	}
	reset, err := parseResetTime(clock[1], now, loc)
	if err != nil {
		return now.Add(fallbackResetSpan).UTC()
	}
	date := strings.Trim(strings.TrimSuffix(text, clock[0]), " ,")
	date = strings.TrimSpace(strings.TrimSuffix(date, " at"))
	if date == "" {
		return reset
	}
	d, err := time.Parse("Jan 2", date)
	if err != nil {
		return now.Add(fallbackResetSpan).UTC()
	}
	local := reset.In(loc)
	dated := time.Date(now.In(loc).Year(), d.Month(), d.Day(), local.Hour(), local.Minute(), 0, 0, loc)
	if !dated.After(now) {
		dated = dated.AddDate(1, 0, 0)
	}
	return dated.UTC()
}

// parseResetTime turns a fragment like "10:20am" or "22:30" into a
// concrete UTC time anchored relative to 'now' in loc. If the parsed
// time-of-day is in the past for today, we roll to tomorrow — Claude
// banners are forward-looking ("resets at HH:MM") so a past time
// implies tomorrow.
func parseResetTime(s string, now time.Time, loc *time.Location) (time.Time, error) {
	// Capture group may include trailing space the greedy \s* picked
	// up before failing the am/pm alternative. time.Parse is strict
	// about extra characters, so normalize first.
	s = strings.TrimSpace(s)
	// Try a series of acceptable layouts. Times in claude banners
	// vary between 24h (no am/pm) and 12h (with am/pm); we accept
	// both.
	layouts := []string{
		"3:04pm", "3:04 pm", "3pm", "3 pm",
		"3:04PM", "3:04 PM", "3PM", "3 PM",
		"15:04", "15",
	}
	var parsed time.Time
	var err error
	for _, l := range layouts {
		parsed, err = time.Parse(l, s)
		if err == nil {
			break
		}
	}
	if err != nil {
		return time.Time{}, err
	}
	// Anchor to today in the banner's zone.
	now = now.In(loc)
	reset := time.Date(
		now.Year(), now.Month(), now.Day(),
		parsed.Hour(), parsed.Minute(), 0, 0, loc,
	)
	if !reset.After(now) {
		reset = reset.AddDate(0, 0, 1)
	}
	return reset.UTC(), nil
}
