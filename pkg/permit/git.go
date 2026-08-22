package permit

import (
	"regexp"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
)

// gitReadonlySubcommands are read-only for every flag except the ones checked
// in gitReadOnly (--output, --ext-diff). Only commands with no write form at
// all belong here; anything with a mutating mode lives below as an explicit
// checker so its specific invocation can be verified.
var gitReadonlySubcommands = bulk.SliceToSet([]string{
	"status", "log", "diff", "show", "blame", "shortlog", "describe",
	"rev-parse", "rev-list", "ls-files", "ls-tree", "ls-remote", "grep",
	"count-objects", "name-rev",
	// Read-only plumbing/porcelain additions with no write form.
	"for-each-ref", // print refs by format
	"show-branch",  // show branch commit history
	"merge-base",   // find common ancestor(s)
	"cat-file",     // read object contents/types/sizes from the object db
	"whatchanged",  // log --raw alias
	"diff-tree",    // plumbing diff of trees/commits
	"diff-files",
	"diff-index",
	"verify-tag",    // verify tag signature, print result
	"verify-commit", // verify commit signature, print result
	"verify-pack",   // validate pack files; reports only
	"cherry",        // show commits missing upstream; no write form
	"check-ref-format",
	"show-ref", // list refs (update-ref is the writer)
})

// gitPreSubcommandFlags are the only flags allowed before a subcommand. -c can
// set pager.<cmd>=... that git then executes, so it disqualifies.
var gitPreSubcommandFlags = bulk.SliceToSet([]string{"--no-pager", "-P"})

// gitActionActions maps a subcommand to its read-only actions, decided by the
// first non-flag argument. A bare invocation falls back to that subcommand's
// default display form, also read-only here. stash is deliberately absent:
// bare `git stash` means `git stash push`.
var gitActionActions = map[string]map[string]struct{}{
	"remote":   bulk.SliceToSet([]string{"show", "get-url"}),
	"reflog":   bulk.SliceToSet([]string{"show"}),
	"worktree": bulk.SliceToSet([]string{"list"}),
}

// isGitExecOrWriteToken reports whether t makes git write a file or run a
// command: --output writes; the rest hand the blob to a config-named command.
func isGitExecOrWriteToken(t string) bool {
	return strings.HasPrefix(t, "--output") || t == "--ext-diff" ||
		t == "--textconv" || t == "--filters"
}

var (
	// gitGrepOpenRe matches -O/--open-files-in-pager short clusters: the
	// argument is parsed as a command line when it holds more than one word.
	gitGrepOpenRe = regexp.MustCompile(`^-[A-Za-z]*O`)
	// gitLsRemoteUploadRe matches --upload-pack/-u, which names a command git runs.
	gitLsRemoteUploadRe = regexp.MustCompile(`^-[a-z]*u`)
)

// isGitGrepExec reports whether an argument makes `git grep` run a pager over
// the matching files.
func isGitGrepExec(t string) bool {
	return gitGrepOpenRe.MatchString(t) || strings.HasPrefix(t, "--open-files-in-pager")
}

// isGitLsRemoteExec reports whether an argument names a command git runs: an
// --upload-pack/-u form, an ext::<cmd> URL, or a quoted arg that collapsed to
// "" in the scan and so cannot be verified.
func isGitLsRemoteExec(t string) bool {
	return strings.HasPrefix(t, "--upload-pack") || gitLsRemoteUploadRe.MatchString(t) ||
		strings.Contains(t, "::") || strings.Contains(t, "\"")
}

// hasGitFlag reports whether short appears in a short-flag cluster (-al), or
// long matches verbatim. Long flags with values (--sort=x) are matched by name.
func hasGitFlag(args []string, short string, long string) bool {
	for _, t := range args {
		if t == long {
			return true
		}
		if strings.HasPrefix(t, "-") && len(t) >= 2 && t[1] != '-' &&
			strings.ContainsRune(t[1:], rune(short[0])) {
			return true
		}
	}
	return false
}

// gitReadOnlyPositionals walks args against a flag allowlist, returning the
// positional arguments and whether every token was recognized. A value is taken
// from the next token only when that token isn't itself a flag, so a write flag
// can never be swallowed as an option value.
func gitReadOnlyPositionals(args []string, shortFlags string, longFlags map[string]struct{}, valueFlags map[string]struct{}) ([]string, bool) {
	var positionals []string
	for i := 0; i < len(args); i++ {
		t := args[i]
		if t == "--" {
			return nil, false // rest is positional by fiat; too loose to verify
		}
		if !strings.HasPrefix(t, "-") || t == "-" {
			positionals = append(positionals, t)
			continue
		}
		if strings.HasPrefix(t, "--") {
			name := t
			eq := -1
			if e := strings.IndexByte(t, '='); e >= 0 {
				name, eq = t[:e], e
			}
			if _, ok := longFlags[name]; !ok {
				return nil, false
			}
			if eq == -1 && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				if _, isVal := valueFlags[name]; isVal {
					i++
				}
			}
			continue
		}
		// short cluster: every character must be a read-only flag or a digit.
		for k := 1; k < len(t); k++ {
			c := t[k]
			if !strings.ContainsRune(shortFlags, rune(c)) && (c < '0' || c > '9') {
				return nil, false
			}
		}
	}
	return positionals, true
}

// gitBranch flags that only select what gets printed. Creation (-t/--track,
// -u/--set-upstream-to), --unset-upstream, deletion and rename all write.
var (
	gitBranchShortRead = "alrvqi"
	gitBranchLongRead  = bulk.SliceToSet([]string{
		"--list", "--all", "--remotes", "--verbose", "--quiet",
		"--ignore-case", "--show-current", "--color", "--no-color", "--column",
		"--no-column", "--abbrev", "--no-abbrev", "--omit-empty", "--contains",
		"--no-contains", "--merged", "--no-merged", "--points-at", "--sort",
		"--format",
	})
	gitBranchValueFlags = bulk.SliceToSet([]string{
		"--contains", "--no-contains", "--merged", "--no-merged", "--points-at",
		"--sort", "--format", "--abbrev", "--color", "--column",
	})
)

// isGitBranchReadOnly reports whether a git branch invocation only reads.
func isGitBranchReadOnly(args []string) bool {
	positionals, ok := gitReadOnlyPositionals(args, gitBranchShortRead, gitBranchLongRead, gitBranchValueFlags)
	if !ok {
		return false
	}
	if len(positionals) == 0 {
		return true // bare `git branch` lists
	}
	// a name is a filter pattern in list mode; anywhere else it creates
	return hasGitFlag(args, "l", "--list")
}

// git tag flags that print only. Creating (-a/-s/-u/-m/-F or a bare name),
// deleting (-d) and force replacement (-f) write.
var (
	gitTagShortRead = "lniv"
	gitTagLongRead  = bulk.SliceToSet([]string{
		"--list", "--ignore-case", "--verify", "--color", "--column",
		"--no-column", "--omit-empty", "--contains", "--no-contains",
		"--merged", "--no-merged", "--points-at", "--sort", "--format",
	})
	gitTagValueFlags = bulk.SliceToSet([]string{
		"--contains", "--no-contains", "--merged", "--no-merged", "--points-at",
		"--sort", "--format", "--color", "--column",
	})
)

// isGitTagReadOnly reports whether a git tag invocation only reads.
func isGitTagReadOnly(args []string) bool {
	positionals, ok := gitReadOnlyPositionals(args, gitTagShortRead, gitTagLongRead, gitTagValueFlags)
	if !ok {
		return false
	}
	if len(positionals) == 0 {
		return true // bare `git tag` lists
	}
	// names are patterns under -l/-n (-n implies listing), tags-to-check under
	// --verify; a name in any other mode creates a tag.
	return hasGitFlag(args, "l", "--list") ||
		hasGitFlag(args, "n", "-n") ||
		hasGitFlag(args, "v", "--verify")
}

// git config query flags only. --edit/-e opens the config in an editor and
// every add/set/unset/rename/remove flag disqualifies.
var (
	gitConfigShortRead = "lz"
	gitConfigLongRead  = bulk.SliceToSet([]string{
		"--list", "--get", "--get-all", "--get-regexp", "--get-urlmatch",
		"--get-color", "--get-colorbool", "--global", "--local", "--system",
		"--worktree", "--file", "--blob", "--show-origin", "--show-scope",
		"--name-only", "--null", "--all", "--regexp", "--value", "--url",
		"--type", "--bool", "--int", "--bool-or-int", "--path", "--expiry-date",
		"--includes", "--no-includes", "--default", "--fixed-value",
	})
	gitConfigValueFlags = bulk.SliceToSet([]string{
		"--file", "--blob", "--type", "--default", "--value", "--url",
	})
	// git config subcommand form (git >= 2.46). edit/set/unset/rename-section and
	// anything else unrecognized fall through to the key check, which rejects:
	// a config key always contains a dot.
	gitConfigReadActions = bulk.SliceToSet([]string{
		"get", "list", "get-all", "get-regexp", "get-urlmatch",
	})
)

// isGitConfigReadOnly reports whether a git config invocation only reads.
func isGitConfigReadOnly(args []string) bool {
	positionals, ok := gitReadOnlyPositionals(args, gitConfigShortRead, gitConfigLongRead, gitConfigValueFlags)
	if !ok {
		return false
	}
	// `git config get <key>`, `git config get-urlmatch <key> <url>`.
	if len(positionals) > 0 {
		if _, act := gitConfigReadActions[positionals[0]]; act {
			return len(positionals) <= 3
		}
	}
	var query bool
	for _, t := range args {
		if strings.HasPrefix(t, "--get") {
			query = true
			break
		}
	}
	// query flags make the positionals keys/patterns/urls rather than values.
	if query || hasGitFlag(args, "l", "--list") {
		return len(positionals) <= 2
	}
	// old form: a lone `<key>` reads it, `<key> <value>` writes it. Requiring
	// the dot rejects bare action words like `edit`.
	return len(positionals) == 0 || (len(positionals) == 1 && strings.Contains(positionals[0], "."))
}

// gitActionReadOnly reports whether a subcommand's first non-flag argument is a
// read-only action; no positional falls back to the default display form.
func gitActionReadOnly(sub string, args []string) bool {
	allowed := gitActionActions[sub]
	if len(allowed) == 0 {
		return false
	}
	for _, t := range args {
		if strings.HasPrefix(t, "-") {
			continue // e.g. `git remote -v`, `git reflog --all`
		}
		_, ok := allowed[t]
		return ok
	}
	return true // default display form: bare remote/reflog/worktree
}

// gitReadOnly reports whether tokens name a verifiably read-only git call.
// Pre-subcommand flags outside the tiny allowlist disqualify; in particular -c
// can set pager.log=<cmd> that git executes. Flags that write a file or run a
// command are rejected globally and per subcommand, and subcommands with both
// read and write forms (branch/tag/config/remote/reflog/worktree) are verified
// against their arguments. Fails safe to the prompt/model path.
func gitReadOnly(tokens []string) bool {
	j := 1
	for j < len(tokens) {
		if _, ok := gitPreSubcommandFlags[tokens[j]]; !ok {
			break
		}
		j++
	}
	if j >= len(tokens) {
		return false // no subcommand
	}
	sub := tokens[j]
	args := slices.Clone(tokens[j+1:])
	for _, t := range args {
		if isGitExecOrWriteToken(t) {
			return false
		}
	}
	switch sub {
	case "grep":
		for _, t := range args {
			if isGitGrepExec(t) {
				return false
			}
		}
	case "ls-remote":
		for _, t := range args {
			if isGitLsRemoteExec(t) {
				return false
			}
		}
	}
	switch sub {
	case "branch":
		return isGitBranchReadOnly(args)
	case "tag":
		return isGitTagReadOnly(args)
	case "config":
		return isGitConfigReadOnly(args)
	}
	if len(gitActionActions[sub]) > 0 {
		return gitActionReadOnly(sub, args)
	}
	_, read := gitReadonlySubcommands[sub]
	return read
}
