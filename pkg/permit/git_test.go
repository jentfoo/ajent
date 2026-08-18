package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGitReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"status", "git status", true},
		{"log", "git log --oneline", true},
		{"diff", "git diff HEAD~1", true},
		{"show", "git show abc123", true},
		{`no subcommand`, `git -c pager.log=cat`, false},

		// branch: read vs create
		{"branch bare lists", "git branch", true},
		{"branch list flag", "git branch --list foo", true},
		{"branch short l", "git branch -l foo", true},
		{"branch verbose", "git branch -avv", true},
		{`branch name creates`, `git branch foo`, false},

		// tag: read vs create
		{"tag bare lists", "git tag", true},
		{"tag list flag", "git tag --list v*", true},
		{"tag num lines", "git tag -n5", true},
		{`tag verify`, `git tag -v v1.0`, true},
		{`tag name creates`, `git tag foo`, false},

		// config: read vs write
		{"config get flag", "git config --get user.name", true},
		{"config list", "git config --list", true},
		{"config old key reads", "git config user.name", true},
		{`config edit writes`, `git config --edit`, false},
		{`config set writes`, `git config user.name Foo`, false},
		{"config subcommand get", "git config get user.name", true},
		{`config action word rejected`, `git config edit`, false},

		// remote / reflog / worktree actions
		{"remote bare lists", "git remote", true},
		{"remote show", "git remote show origin", true},
		{"remote get url", "git remote get-url origin", true},
		{`remote add writes`, `git remote add origin git@x`, false},
		{"reflog show", "git reflog show HEAD", true},
		{"worktree list", "git worktree list", true},
		{`stash bare is push`, `git stash`, false},

		// exec / write flags rejected globally and per subcommand
		{`output flag`, `git diff --output out.patch`, false},
		{`ext diff`, `git log --ext-diff`, false},
		{"grep open pager", "git grep -O vim foo", false},
		{"ls remote upload pack", "git ls-remote --upload-pack=sh origin", false},
		{"ls remote ext url", "git ls-remote 'ext::sh -c cat% .'", false},

		// pre-subcommand pager flag
		{`pager config executes`, `git -c pager.log=less status`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, gitReadOnly(segmentTokens(c.in)))
		})
	}
}

func TestIsGitBranchReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"list short", "-l foo", true},
		{"list long glob", "--list 'foo*'", true},
		{"verbose all", "-avv", true},
		// creation flags write
		{"track creates", "--track foo", false},
		{"set upstream writes", "--set-upstream-to=origin/main", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isGitBranchReadOnly(stringsFields(c.in)))
		})
	}
}

func TestIsGitTagReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"num lines", "-n5 v*", true},
		{"list glob", "--list 'v1.*'", true},
		{"verify", "--verify v1.0", true},
		// annotated / delete write
		{"annotated creates", "-a -m msg v1.0", false},
		{"delete writes", "-d v1.0", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isGitTagReadOnly(stringsFields(c.in)))
		})
	}
}

func TestIsGitConfigReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"get flag", "--get user.name", true},
		{"list short", "-l", true},
		{"lone dotted key reads", "user.name", true},
		// two positionals write a value
		{"key and value writes", "user.name Foo", false},
		{"action word rejected", "edit", false}, // action word without a dot is rejected
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isGitConfigReadOnly(stringsFields(c.in)))
		})
	}
}

func TestGitActionReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"show origin", "show origin", true},
		{"get url", "get-url origin", true},
		// mutating actions fail
		{"add writes", "add origin git@x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, gitActionReadOnly("remote", stringsFields(c.in)))
		})
	}
}

func TestIsGitExecOrWriteToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"output flag", "--output=out.patch", true},
		{"ext diff", "--ext-diff", true},
		{"textconv", "--textconv", true},
		{"filters", "--filters", true},
		{"stat is read only", "--stat", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isGitExecOrWriteToken(c.in))
		})
	}
}

// stringsFields splits a line into tokens for the verification helpers.
func stringsFields(s string) []string {
	return tokenizeRaw(s)
}
