package permit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDenyMatches(t *testing.T) {
	t.Parallel()

	// every shape here executes the denied command while hiding its head from
	// a naive token match
	cases := []struct {
		name string
		cmds []string
		in   string
	}{
		{"compound_component", []string{"git stash"}, "cd /x && git stash push -m wip"},
		{"nested_sh_c", []string{"git push"}, `sh -c "git push --force"`},
		{"nested_bash_c", []string{"git push"}, `bash -c 'git push'`},
		{"nested_eval", []string{"git push"}, `eval git push`},
		{"nested_sudo_sh_c", []string{"rm"}, `sudo sh -c "rm -rf x"`},
		{"nested_sudo_opt_sh_c", []string{"rm"}, `sudo -u root sh -c "rm -rf x"`},
		{"nested_env_flag_sh_c", []string{"rm"}, `env -i sh -c "rm -rf x"`},
		// quoting or escaping the head cannot slip a listed command past the list
		{"quoted_head", []string{"git push"}, `"git" push`},
		{"single_quoted_head", []string{"git push"}, `'git' push`},
		{"quoted_subcommand", []string{"git push"}, `git "push"`},
		{"escaped_char", []string{"git push"}, `git pu\sh`},
		{"escaped_head", []string{"git push"}, `gi\t push`},
		{"ansi_c_quoted_head", []string{"rm"}, `$'rm' -rf x`},
		{"locale_quoted_head", []string{"rm"}, `$"rm" -rf x`},
		{"ansi_c_after_launcher", []string{"rm"}, `nohup $'rm' -rf x`},
		{"quoted_in_compound", []string{"git push"}, `cd /x && git "push"`},
		// leading env assignments are skipped, the prefixed head refusing like
		// its bare form
		{"env_prefixed_head", []string{"rm"}, "FOO=bar rm -rf /"},
		{"env_preload_prefix", []string{"rm"}, "LD_PRELOAD=./x.so rm -rf /"},
		{"env_stacked_prefixes", []string{"rm"}, "A=1 B=2 rm x"},
		{"env_prefixed_compound", []string{"rm"}, "cd /x && FOO=bar rm -rf /"},
		{"env_prefixed_payload", []string{"git push"}, `sh -c "FOO=bar git push"`},
		{"env_launcher_head", []string{"git"}, "env git push origin main"},
		{"env_assignment_past_launcher", []string{"git"}, "env FOO=1 git push"},
		{"env_flag_past_launcher", []string{"git"}, "env -i git push"},
		// subshell, brace group and loop bodies run their inner commands
		{"subshell_body", []string{"rm"}, "( rm -rf x )"},
		{"subshell_compound_body", []string{"rm"}, "( cd /tmp && rm -rf x )"},
		{"subshell_nested_body", []string{"rm"}, "( ( rm -rf x ) )"},
		{"brace_group_body", []string{"rm"}, "{ rm -rf x; }"},
		{"loop_body", []string{"git push"}, "for f in *; do git push; done"},
		{"while_body", []string{"rm"}, "while true; do rm x; done"},
		{"if_body", []string{"rm"}, "if true; then rm x; fi"},
		{"case_tail", []string{"git push"}, "case x in y) git push;; esac"},
		{"case_paren_pattern", []string{"git push"}, "case x in (y) git push;; esac"},
		{"function_body", []string{"rm"}, "f() { rm -rf x; }; f"},
		// in-line substitution executes its contents
		{"command_substitution", []string{"git push"}, "$(git push)"},
		{"substitution_mid_line", []string{"rm"}, "echo pre $(rm -rf x) post"},
		{"backtick_substitution", []string{"git push"}, "`git push`"},
		{"double_quoted_substitution", []string{"git push"}, `echo "$(git push)"`},
		{"double_quoted_backtick", []string{"git push"}, "echo \"`git push`\""},
		{"process_substitution", []string{"git push"}, "diff <(git push) <(echo x)"},
		{"substitution_in_subshell", []string{"rm"}, "( echo $(rm -rf x) )"},
		// transparent launchers wrap the denied command
		{"nohup_launcher", []string{"git push"}, "nohup git push origin main"},
		{"timeout_launcher", []string{"rm"}, "timeout 10 rm -rf x"},
		{"timeout_opt_launcher", []string{"rm"}, "timeout -k 5 10 rm -rf x"},
		{"sudo_launcher", []string{"rm"}, "sudo rm -rf x"},
		{"sudo_opt_launcher", []string{"rm"}, "sudo -u root rm -rf x"},
		{"nice_launcher", []string{"make"}, "nice -n 5 make install"},
		{"xargs_launcher", []string{"rm"}, "find . | xargs rm"},
		{"time_launcher", []string{"rm"}, "time rm -rf x"},
		{"launcher_named_entry", []string{"env"}, "env FOO=1 git push"},
		// a heredoc body is scanned when an interpreter reads it, quoted
		// delimiters protecting nothing
		{"heredoc_sh_body", []string{"rm"}, "sh <<EOF\nrm -rf /\nEOF"},
		{"heredoc_bash_body", []string{"git push"}, "bash <<E\ngit push\nE"},
		{"heredoc_quoted_delim", []string{"rm"}, "sh <<'EOF'\nrm -rf /\nEOF"},
		{"heredoc_after_then", []string{"rm"}, "if :; then sh <<E\nrm x\nE\nfi"},
		{"heredoc_body_nested", []string{"git push"}, "sh <<E\nsh -c 'git push'\nE"},
		{"heredoc_nohup_sh", []string{"rm"}, "nohup sh <<E\nrm -rf x\nE"},
		{"heredoc_in_subshell", []string{"rm"}, "( sh <<E\nrm -rf x\nE )"},
		// an unquoted delimiter leaves substitution live whatever reads the
		// body, since the shell expands it before the reader sees the text
		{"heredoc_cat_substitution", []string{"rm"}, "cat <<E\nrm status: $(rm -rf x)\nE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.True(t, DenyMatches(bashCall(c.in), c.cmds), c.in)
		})
	}
}

func TestDenyMatchesNearMissesStayUnmatched(t *testing.T) {
	t.Parallel()

	// token-exact, never substring: near misses never refuse
	cmds := []string{"rm", "git push"}
	for _, in := range []string{
		"git pushx",
		"gitx push",
		"rmdir x",
		"FOO=bar ls -la",
		"( ls -la )",
		"{ echo hi; }",
		"for f in *; do ls -l $f; done",
		"echo $(git status)",
		"echo $(pwd) rm",
		"echo \"`date`\"",
		"'$(git push)'",
		"nohup ls",
		"timeout 10 ls",
		"env FOO=1 ls",
		"time ls",
		"xargs grep -c pattern",
		"man sh",
		"cat <<E\nrm -rf /\nE",
		"python - <<E\nrm -rf /\nE",
		"tee <<E\nrm -rf /\nE",
		"cat <<'E'\n$(rm -rf x)\nE",
		"printf 'rm -rf /' | sh",
	} {
		assert.False(t, DenyMatches(bashCall(in), cmds), in)
	}
}

func TestDenyMatchesToolNames(t *testing.T) {
	t.Parallel()

	// exact names and MCP server namespaces deny any non-bash tool
	assert.True(t, DenyMatches(call("write", `{}`), []string{"write"}))
	assert.True(t, DenyMatches(call("sectool__poll", `{}`), []string{"sectool"}))
	assert.False(t, DenyMatches(call("github__repo", `{}`), []string{"sectool"}))
	assert.False(t, DenyMatches(bashCall("rm -rf x"), []string{"write"}))
}

func TestDenyMatchesDeepNestingTerminates(t *testing.T) {
	t.Parallel()

	// a pathologically nested line stays bounded instead of exhausting the
	// stack; depth past the cap simply stops descending
	deep := strings.Repeat("(", 10_000) + "rm x" + strings.Repeat(")", 10_000)
	assert.False(t, DenyMatches(bashCall(deep), []string{"rm"}))

	shallow := strings.Repeat("( ", 60) + "rm x" + strings.Repeat(" )", 60)
	assert.True(t, DenyMatches(bashCall(shallow), []string{"rm"}))
}

func TestStripDollarQuotes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ansi_c_word", `$'rm' -rf x`, `'rm' -rf x`},
		{"locale_word", `$"rm" -rf x`, `"rm" -rf x`},
		{"mid_word_stays", `foo$'bar' x`, `foo$'bar' x`},
		{"after_space", `nohup $'rm' x`, `nohup 'rm' x`},
		{"command_subst_stays", `$(git push)`, `$(git push)`},
		{"no_dollar", "ls -la", "ls -la"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, stripDollarQuotes(c.in))
		})
	}
}

func TestShellBodies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		mode bodyMode
		want []string
	}{
		{"subshell_shell_mode", "( rm x )", bodyShell, []string{" rm x ", ""}},
		{"subshell_subst_mode", "( rm x )", bodySubst, nil},
		{"command_subst_both", "$(git push)", bodySubst, []string{"git push"}},
		{"backtick_both", "`git push`", bodySubst, []string{"git push"}},
		{"procsub_both", "<(git push)", bodySubst, []string{"git push"}},
		{"double_quoted_subst", `echo "$(git push)"`, bodySubst, []string{"git push"}},
		{"single_quoted_inert", `'$(git push)'`, bodyShell, nil},
		{"case_tail", "case x in y) git push;; esac", bodyShell,
			[]string{" git push;; esac"}},
		{"case_paren_tail", "case x in (y) git push;; esac", bodyShell,
			[]string{"y", " git push;; esac"}},
		{"function_tail", "f() { rm x; }", bodyShell, []string{"", " { rm x; }"}},
		{"subst_tail_is_argument", "echo $(date) rm", bodyShell, []string{"date"}},
		{"nested_groups", "( ( rm x ) )", bodyShell,
			[]string{" ( rm x ) ", ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, shellBodies(c.in, c.mode))
		})
	}
}

func TestInterpreterPayloads(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"sh_c", `sh -c "git push"`, []string{`git push`}},
		{"bare_sh_reads_stdin", "sh <<E", nil},
		{"eval_joins", `eval git push`, []string{"git push"}},
		{"keyword_then_sh", "then sh -c rm", []string{"rm"}},
		{"sudo_sh_c", `sudo sh -c "rm -rf x"`, []string{`rm -rf x`}},
		{"env_sh_c", `env sh -c "rm -rf x"`, []string{`rm -rf x`}},
		{"env_flag_sh_c", `env -i sh -c "rm -rf x"`, []string{`rm -rf x`}},
		{"sudo_opt_sh_c", `sudo -u root sh -c "rm -rf x"`, []string{`rm -rf x`}},
		{"nohup_bash_c", `nohup bash -c 'rm x'`, []string{`rm x`}},
		{"plain_command", "git push", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, interpreterPayloads(c.in))
		})
	}
}

func TestResolveDenyArgv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "rm -rf x", []string{"rm", "-rf", "x"}},
		{"keywords_skipped", "{ rm -rf x", []string{"rm", "-rf", "x"}},
		{"loop_keywords", "do git push", []string{"git", "push"}},
		{"env_skipped", "FOO=1 rm x", []string{"rm", "x"}},
		{"keyword_then_env", "do FOO=1 rm x", []string{"rm", "x"}},
		{"nohup_unwrapped", "nohup git push", []string{"git", "push"}},
		{"timeout_unwrapped", "timeout 10 rm x", []string{"rm", "x"}},
		{"env_launcher_stays", "env git push", []string{"env", "git", "push"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, resolveDenyArgv(tokenizeRaw(c.in)))
		})
	}
}
