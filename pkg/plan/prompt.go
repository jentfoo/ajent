package plan

import "strings"

// Compaction focus per phase, handed to the summariser so a phase keeps what it
// still needs rather than a balanced digest of everything.
const (
	implementFocus = "Focus on the implementation work: files changed, approaches tried, " +
		"decisions made, and what remains unfinished. Reproduce any unfinished plan items verbatim."
	reviewFocus = "Focus on the review so far: files inspected, issues found, and conclusions reached."
)

// planningContract is appended to the user's goal as the planner's role. It is a
// separate block so the echoed line stays the user's own words.
func planningContract() string {
	return "You are planning, not implementing. Explore the codebase to ground the plan in what " +
		"is actually there, reusing existing helpers and following the conventions you find. " +
		"When a genuine design fork appears — one with real trade-offs, or a fact you cannot " +
		"discover — put it to the user with `" + AskUserTool + "` rather than guessing.\n\n" +
		"The plan is handed to a SEPARATE model with NO prior context: it will not see this " +
		"conversation, this codebase exploration, or anything the user told you. Every file path, " +
		"interface, constraint and acceptance criterion must be carried in the plan text itself. " +
		"Describe interfaces and function signatures where they pin the design down, but do not " +
		"write the implementation for it.\n\n" +
		"Aim for a well-integrated, maintainable change. Call `" + DevImplementTool + "` once the " +
		"plan is complete and every clarifying question is resolved. Do not edit anything yourself."
}

// implementKickoff is the first and only message of a fresh implementation
// branch: the approved plan, plus this round's revision instructions.
func implementKickoff(plan, revision string) string {
	var b strings.Builder
	b.WriteString("You have no prior context. Everything you need is below.\n\n" +
		"Implement the plan precisely. Follow the conventions of the files you touch, and " +
		"verify your work the way the project does (its build and test commands).\n\n" +
		"Call `" + DevReviewTool + "` with a short summary when the work is done. If you stop " +
		"without calling it, review begins anyway, so stop only when the work is finished.\n\n")
	b.WriteString("<plan>\n" + strings.TrimSpace(plan) + "\n</plan>\n")
	if revision != "" {
		b.WriteString("\nA previous attempt was reviewed and found incomplete or incorrect. " +
			"Address these points specifically; they describe work still outstanding.\n\n" +
			"<revision_instructions>\n" + strings.TrimSpace(revision) + "\n</revision_instructions>\n")
	}
	return b.String()
}

// reviewKickoff opens a review round with the approved plan and the working
// tree's current state.
func reviewKickoff(plan, gitStatus, diffStat, summary string) string {
	var b strings.Builder
	b.WriteString("Verify the implementation against the plan below. The implementation " +
		"conversation is deliberately absent, so re-read every file you need to judge rather " +
		"than assuming what was done. Do not edit anything.\n\n" +
		"When you are done, either call `" + DevCompleteTool + "` if the change fulfils the plan " +
		"and is free of defects, or call `" + DevReviseTool + "` with specific, actionable " +
		"instructions. Those instructions go to a model with NO prior context and no background, " +
		"so each one must be self-contained and name the files it concerns.\n\n")
	b.WriteString("<plan>\n" + strings.TrimSpace(plan) + "\n</plan>\n")
	b.WriteString("\nThis is the plan as the user approved it, which may differ from the draft.\n")
	b.WriteString("\n<git_status>\n" + fallback(gitStatus, "(no changes reported)") + "\n</git_status>\n")
	if diffStat != "" {
		b.WriteString("\n<git_diff_stat>\n" + diffStat + "\n</git_diff_stat>\n")
	}
	if summary != "" {
		b.WriteString("\n<implementation_summary>\n" + strings.TrimSpace(summary) +
			"\n</implementation_summary>\n")
	}
	return b.String()
}

// execContinue restarts an implementation turn that died on a provider error.
func execContinue() string {
	return "The previous turn ended with an API error before it finished. Continue the " +
		"implementation from where it stopped, without repeating work already completed, and " +
		"call `" + DevReviewTool + "` when the work is done."
}

// capReport closes a workflow that ran out of revision rounds, naming what the
// last review still wanted.
func capReport(lastRound string) string {
	msg := "reached the revision limit (" + itoa(maxRevisions) + "); ending the workflow"
	if lastRound != "" {
		msg += ". The last review asked for: " + strings.TrimSpace(lastRound)
	}
	return msg
}

// fallback returns s, or alt when s is blank.
func fallback(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}
