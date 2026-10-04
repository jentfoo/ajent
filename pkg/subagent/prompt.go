package subagent

import (
	"fmt"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
)

// childBuiltinTools names the built-in read-only tools advertised to a child.
// The four git readers are present only inside a work tree, advertising them
// elsewhere inviting calls that no tool answers.
func childBuiltinTools(inRepo bool) string {
	list := "read, grep, find, ls"
	if inRepo {
		return list + ", git_status, git_log, git_show, git_diff"
	}
	return list
}

const childContractBase = `You are an isolated research sub-agent running as a background task of a coding agent.

Constraints:
- You have ONLY read-only tools: %s and any MCP tool marked read-only. They are typed tool calls; do not try to invoke them via shell.
- You CANNOT edit files or run shell commands. Do not attempt destructive operations.
- Investigate thoroughly, then STOP.

Output:
Your FINAL assistant message must be a single, self-contained summary of everything you discovered. It will be the ONLY thing returned to the calling agent. Include conclusions, key file paths with line numbers, and any caveats or uncertainties. Do not emit tool calls in that final message. Be clear and concise.`

// childContract is the system snippet appended to every child's prompt: read-only
// constraints plus the output contract. Its final assistant message is the whole
// return value, so it must be self-contained and free of tool calls.
func childContract(inRepo bool) string {
	return fmt.Sprintf(childContractBase, childBuiltinTools(inRepo))
}

// continueNudge asks a child whose final message carried only thinking to emit
// its summary as plain text, bounded by maxContinueAttempts.
const continueNudge = `Continue. Your previous message had no summary text (only internal reasoning). Now output the final, self-contained summary as plain text with no tool calls.`

// truncatedNudge asks a child whose last message was cut off to stop and
// summarise what it has already found.
const truncatedNudge = `Your previous message was cut off before completion (the output or step limit was reached). Output your final, self-contained summary of everything discovered so far as plain text with no tool calls.`

// nudgeFor picks the wrap-up prompt: a cut-off message gets the truncation
// variant, anything else blank gets the reasoning one.
func nudgeFor(last *llm.Message) string {
	if truncated(last) {
		return truncatedNudge
	}
	return continueNudge
}

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
func childSnippets(inRepo bool) []string { return []string{childContract(inRepo)} }
