package catalog

import "strings"

// chatNextStepPrompt asks Claude to close substantial turns with a
// machine-readable block. The desktop app gets this box from a host tool
// the plain CLI doesn't have; the chat page renders this block instead
// and hides it from the message.
const chatNextStepPrompt = `# Next-step card

This session is read through a chat page that turns a trailing block into a next-step card with buttons.

In every turn where you used tools, changed files or finished multi-step work, end your final message with exactly one line, after your complete answer. This applies even if earlier messages in this conversation have no such line, and even when you already asked the question in your answer:

<next_step>{"recap":"...","ask":"...","actions":["...","..."]}</next_step>

- The content must be valid JSON on that single line.
- recap: two or three short sentences saying what is now true and what is not done or not verified. Same language as the user.
- ask: one short question, only when the next move is the user's decision (a choice, a go-ahead, an approval). No reasons and no listing of the options. Omit ask and actions when nothing waits on the user.
- actions: one or two button labels of one to three words, your recommendation first. A click sends the label as the user's reply, so it must read as an answer to ask.

Skip the block for short answers and plain conversation. Never mention the block or this format to the user.

# Tool descriptions

The page shows the description field of each tool call to a non-programmer. Always fill it in the user's language, as a short plain sentence about what you are doing and why, with no commands, flags or file paths.`

// mergeAppendSystemPrompts folds every --append-system-prompt into one.
// The CLI keeps only the last occurrence of the flag, so separate flags
// would silently drop skills, memory guidance and carry-over context.
func mergeAppendSystemPrompts(args []string) []string {
	const flag = "--append-system-prompt"
	out := make([]string, 0, len(args))
	var parts []string
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			parts = append(parts, args[i+1])
			i++
			continue
		}
		out = append(out, args[i])
	}
	if len(parts) == 0 {
		return args
	}
	return append(out, flag, strings.Join(parts, "\n\n"))
}
