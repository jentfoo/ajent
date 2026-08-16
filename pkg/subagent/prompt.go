package subagent

import "strings"

// childContract is the system snippet appended to every child's prompt: read-only
// constraints plus the output contract. Its final assistant message is the whole
// return value, so it must be self-contained and free of tool calls.
const childContract = `You are an isolated research sub-agent running as a background task of a coding agent.

Constraints:
- You have ONLY read-only tools: read, grep, find, ls and any MCP tool marked read-only. They are typed tool calls; do not try to invoke them via shell.
- You CANNOT edit files or run shell commands. Do not attempt destructive operations.
- Investigate thoroughly, then STOP.

Output:
Your FINAL assistant message must be a single, self-contained summary of everything you discovered. It will be the ONLY thing returned to the calling agent. Include conclusions, key file paths with line numbers, and any caveats or uncertainties. Do not emit tool calls in that final message. Be clear and concise.`

// continueNudge asks a child whose final message carried only thinking to emit
// its summary as plain text; bounded by maxContinueAttempts.
const continueNudge = `Continue. Your previous message had no summary text (only internal reasoning). Now output the final, self-contained summary as plain text with no tool calls.`

// taskPrompt assembles a child's first input from the delegated investigation
// and any extra instructions.
func taskPrompt(task, instructions string) string {
	body := "Task:\n" + strings.TrimSpace(task)
	if s := strings.TrimSpace(instructions); s != "" {
		body = "Extra instructions:\n" + s + "\n\n" + body
	}
	return body
}

// childSnippets is the system-snippet slice a child agent gets.
func childSnippets() []string { return []string{childContract} }
