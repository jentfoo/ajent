package permit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

func TestCallPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"write_args", `{"path":"a/b.go","content":"x"}`, "a/b.go"},
		{"edit_args", `{"path":"/tmp/x","edits":[]}`, "/tmp/x"},
		{"missing_field", `{"content":"x"}`, ""},
		{"malformed_json", `{`, ""},
		{"wrong_type", `{"path":42}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, callPath([]byte(c.in)))
		})
	}
}

func TestWriteScopeContains(t *testing.T) {
	t.Parallel()

	s := writeScope{cwd: "/work", roots: []string{"/work", "/tmp"}}
	cases := []struct {
		name string
		full string
		want bool
	}{
		{"exact_root", "/work", true},
		{"nested_path", "/work/pkg/a.go", true},
		{"second_root", "/tmp/scratch", true},
		{"sibling_prefix", "/workspace/a.go", false},
		{"tmp_sibling_prefix", "/tmpfoo/a", false},
		{"outside", "/etc/passwd", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, s.contains(c.full))
		})
	}

	t.Run("no_roots", func(t *testing.T) {
		assert.False(t, writeScope{}.contains("/work/a.go"))
	})
}

func TestWriteScopeAllows(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	tmp := t.TempDir()
	outside := t.TempDir()
	s := newWriteScope(cwd, tmp)

	// a symlink inside cwd pointing out must resolve out of scope
	link := filepath.Join(cwd, "escape")
	require.NoError(t, os.Symlink(outside, link))

	cases := []struct {
		name string
		call agent.ToolCall
		want bool
	}{
		{"relative_write", call("write", `{"path":"a/b.go"}`), true},
		{"absolute_in_cwd", call("write", `{"path":"`+cwd+`/a.go"}`), true},
		{"edit_in_tmp", call("edit", `{"path":"`+tmp+`/x"}`), true},
		{"cwd_itself", call("write", `{"path":"."}`), true},
		{"outside_absolute", call("write", `{"path":"`+outside+`/a.go"}`), false},
		{"home_path", call("write", `{"path":"~/.ssh/config"}`), false},
		{"parent_escape", call("write", `{"path":"../a.go"}`), false},
		{"symlink_escape", call("write", `{"path":"escape/a.go"}`), false},
		{"missing_path", call("write", `{}`), false},
		{"bash_never", bashCall("rm -rf /"), false},
		{"read_never", call("read", `{"path":"a.go"}`), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, s.allows(c.call))
		})
	}

	t.Run("unset_scope", func(t *testing.T) {
		assert.False(t, writeScope{}.allows(call("write", `{"path":"a.go"}`)))
	})
}

func TestWriteScopeAllowsCommand(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	outside := t.TempDir()
	s := newWriteScope(cwd)

	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"mkdir_relative", "mkdir build", true},
		{"mkdir_parents_flag", "mkdir -p a/b/c", true},
		{"mkdir_absolute_in_cwd", "mkdir " + cwd + "/x", true},
		{"rmdir_relative", "rmdir build", true},
		{"rmdir_multiple", "rmdir a b", true},
		{"mkdir_mode_flag", "mkdir -m 755 a", true},
		{"mkdir_mode_joined", "mkdir --mode=755 a", true},
		{"mkdir_mode_long_flag", "mkdir --mode 755 a", true},
		{"quoted_path", `mkdir "my dir"`, true},
		{"chained_with_read", "ls && mkdir build", true},
		{"pure_readonly", "ls -la", true},

		{"mkdir_outside", "mkdir " + outside + "/x", false},
		{"mkdir_home", "mkdir ~/x", false},
		{"mkdir_parent_escape", "mkdir ../x", false},
		{"mkdir_absolute_system", "mkdir /etc/x", false},
		{"mkdir_no_args", "mkdir", false},
		{"mkdir_only_flags", "mkdir -p", false},
		{"mkdir_mode_without_path", "mkdir -m 755", false}, // 755 is a value, not a path
		{"variable_path", "mkdir $HOME/x", false},
		{"glob_path", "rmdir *", false},
		// brace expansion splits one token into several words before mkdir runs, so a
		// leading brace can smuggle an absolute path past a token that looks relative
		{"brace_absolute_escape", "mkdir {/etc/x,/tmp/y}", false},
		{"brace_escape_with_flag", "mkdir -p {/etc/x,/tmp/y}", false},
		{"brace_rmdir_escape", "rmdir {/etc/x,/tmp/y}", false},
		{"brace_in_scope_still_refused", "mkdir build/{a,b}", false},
		{"redirect_fails_closed", "mkdir a > f", false},
		{"substitution_fails_closed", "mkdir $(cat f)", false},
		{"chained_with_writer", "mkdir a && rm -rf b", false},
		{"unlisted_writer", "touch a", false},
		{"sed_in_place", "sed -i s/a/b/ f", false},
		{"env_prefixed", "PATH=/x mkdir a", false},
		// a cd rebases every relative path after it, so its target is checked too
		{"cd_in_scope", "cd build && mkdir x", true},
		{"cd_nested_in_scope", "cd a && cd b && mkdir x", true},
		{"cd_then_read", "cd build && ls && mkdir x", true},
		{"cd_root_launders", "cd / && mkdir x", false},
		{"cd_home_launders", "cd ~ && mkdir evil", false},
		{"cd_semicolon_launders", "cd /etc; rmdir ssl/x", false},
		{"cd_outside_launders", "cd " + outside + " && mkdir x", false},
		{"cd_parent_launders", "cd .. && mkdir x", false},
		{"cd_bare_goes_home", "cd && mkdir x", false},
		{"cd_dash_unknowable", "cd - && mkdir x", false},
		{"cd_git_dir", "cd .git && mkdir x", false},
		// the cd may not run, so a path must be in scope from before and after it
		{"cd_then_dotdot_escape", "cd sub && mkdir ../../x", false},
		{"cd_then_dotdot_ambiguous", "cd sub && mkdir ../x", false},
		// rmdir -p only walks the operand's own components, so a relative one is bounded
		{"rmdir_parents_relative", "rmdir -p a/b/c", true},
		{"rmdir_parents_long", "rmdir --parents a/b/c", true},
		{"rmdir_parents_absolute", "rmdir -p /s/root/a", false},
		{"rmdir_parents_dotdot", "rmdir -p ../a/b", false},
		{"rmdir_parents_flag_after", "rmdir a/b -p", true},
		{"mkdir_parents_still_ok", "mkdir -p a/b/c", true},
		// an unknown flag may consume a value, so it cannot be told from an operand
		{"unknown_flag", "mkdir --frobnicate a", false},
		{"bare_double_dash", "mkdir -- a", false},
		// VCS metadata executes on the next git invocation
		{"mkdir_git_dir", "mkdir .git/hooks", false},
		{"rmdir_git_dir", "rmdir .git/x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, s.allowsCommand(c.cmd))
		})
	}

	t.Run("unset_scope", func(t *testing.T) {
		assert.False(t, writeScope{}.allows(bashCall("mkdir build")))
	})
	t.Run("routed_through_allows", func(t *testing.T) {
		assert.True(t, s.allows(bashCall("mkdir build")))
		assert.False(t, s.allows(bashCall("mkdir /etc/x")))
	})
}

// TestWriteScopeAllowsBashCwd covers the cwd bash accepts alongside command: it
// rebases every relative path, so the scope must judge the directory it runs in.
func TestWriteScopeAllowsBashCwd(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(cwd, "sub"), 0o755))
	s := newWriteScope(cwd)

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"no_cwd_declared", `{"command":"mkdir x"}`, true},
		{"cwd_in_scope", `{"command":"mkdir x","cwd":"` + cwd + `/sub"}`, true},
		{"cwd_relative_in_scope", `{"command":"mkdir x","cwd":"sub"}`, true},
		{"cwd_outside", `{"command":"mkdir x","cwd":"` + outside + `"}`, false},
		{"cwd_system", `{"command":"mkdir x","cwd":"/etc"}`, false},
		{"cwd_root", `{"command":"rmdir x","cwd":"/"}`, false},
		{"cwd_home", `{"command":"mkdir x","cwd":"~"}`, false},
		{"cwd_git_dir", `{"command":"mkdir x","cwd":".git"}`, false},
		{"cwd_escapes_out_then_in", `{"command":"mkdir x","cwd":"../"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, s.allows(call("bash", c.in)))
		})
	}
}

// TestWriteScopeExcludesVCS pins that repository metadata is never auto-written:
// a hook or config alias there is code that runs at the next git invocation.
func TestWriteScopeExcludesVCS(t *testing.T) {
	t.Parallel()

	s := newWriteScope(t.TempDir())
	for _, p := range []string{
		".git/hooks/pre-commit", ".git/config", "sub/.git/config",
		".hg/hgrc", ".svn/x", "./.git/hooks/post-merge",
	} {
		t.Run(p, func(t *testing.T) {
			assert.False(t, s.allows(call("write", `{"path":"`+p+`"}`)))
			assert.False(t, s.allows(call("edit", `{"path":"`+p+`"}`)))
		})
	}

	t.Run("gitignore_still_writable", func(t *testing.T) {
		assert.True(t, s.allows(call("write", `{"path":".gitignore"}`)))
	})
}
