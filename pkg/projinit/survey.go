package projinit

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/tools"
)

const (
	agentsFile    = "AGENTS.md"
	maxCodeAgents = 4
	// file counts earning a second, third and fourth codebase agent
	smallRepo  = 50
	mediumRepo = 200
	largeRepo  = 600

	maxUnits     = 24    // upper bound on the paths named across every slice
	maxExpands   = 8     // how many times an oversized directory may be opened up
	maxWalkFiles = 20000 // bound on the non-git fallback walk

	gitTimeout = 5 * time.Second // matches the plan workflow's git calls
)

// ownedElsewhere names the root files stage 1 or the build agent already reads.
var ownedElsewhere = bulk.SliceToSet([]string{
	agentsFile, "CONTRIBUTING.md", "LICENSE", "LICENSE.md", "LICENSE.txt",
	"Makefile", "go.sum", "package-lock.json", "yarn.lock",
})

// docFiles returns the files /init reads for itself — every README plus an
// existing AGENTS.md — and whether that AGENTS.md was among them.
func docFiles(cwd string) ([]string, bool) {
	var out []string
	matches, _ := filepath.Glob(filepath.Join(cwd, "README*"))
	slices.Sort(matches)
	for _, m := range matches {
		if isFile(m) {
			out = append(out, filepath.Base(m))
		}
	}
	// the existing file lands last, nearest the instruction that corrects it
	if agents := filepath.Join(cwd, agentsFile); isFile(agents) {
		return append(out, agentsFile), true
	}
	return out, false
}

// surveyTasks returns every stage 2 task: one build and test survey, then one
// per disjoint codebase slice.
func surveyTasks(cwd string) []string {
	parts := codeSlices(cwd)
	out := make([]string, 0, len(parts)+1)
	out = append(out, buildTask)
	for _, p := range parts {
		out = append(out, codeTask(p))
	}
	return out
}

// codeSlices partitions the working directory into disjoint path sets, one per
// codebase sub-agent. The count scales with the tree so a small repository gets
// one thorough pass rather than a fragmented handful.
func codeSlices(cwd string) [][]string {
	files := surveyable(repoFiles(cwd))
	if len(files) == 0 {
		return [][]string{{"."}}
	}
	n := agentCount(len(files))
	us := sliceUnits(files, n)
	if n = min(n, len(us)); n <= 0 {
		return [][]string{{"."}}
	}

	// largest first into the least loaded slice, so the halves stay comparable
	slices.SortStableFunc(us, func(a, b unit) int { return b.count - a.count })
	out := make([][]string, n)
	load := make([]int, n)
	for _, u := range us {
		i := leastLoaded(load)
		out[i] = append(out[i], u.label())
		load[i] += u.count
	}
	return out
}

// agentCount maps a repository's file count onto how many codebase agents it earns.
func agentCount(files int) int {
	switch {
	case files < smallRepo:
		return 1
	case files < mediumRepo:
		return 2
	case files < largeRepo:
		return 3
	default:
		return maxCodeAgents
	}
}

// unit is one indivisible piece of the tree a slice can be given.
type unit struct {
	path  string // a directory path, or "" for the files sitting directly in dir
	dir   string // the directory the loose-file form describes
	count int
}

// label renders a unit for a task's path list.
func (u unit) label() string {
	if u.path != "" {
		return u.path + "/"
	}
	if u.dir == "" {
		return "the files at the repository root"
	}
	return "the files directly in " + u.dir + "/"
}

// sliceUnits returns the pieces to divide between n agents: top-level
// directories, with any that would swamp a slice replaced by its own children so
// the division tracks where the code actually is.
func sliceUnits(files []string, n int) []unit {
	us := group(files, "")
	target := len(files) / max(n, 1)
	for range maxExpands {
		i := largest(us)
		if i < 0 || us[i].path == "" || us[i].count <= target || len(us) >= maxUnits {
			break
		}
		kids := group(files, us[i].path+"/")
		if len(kids) < 2 { // nothing to gain from descending into a single child
			break
		}
		us = slices.Concat(us[:i:i], us[i+1:], kids)
	}
	return us
}

// group buckets the files under prefix by their next path segment; those sitting
// directly in prefix collapse into one unit rather than a scatter of singletons.
func group(files []string, prefix string) []unit {
	counts := make(map[string]int)
	var order []string
	var loose int
	for _, f := range files {
		rest, ok := strings.CutPrefix(f, prefix)
		if !ok {
			continue
		}
		head, _, nested := strings.Cut(rest, "/")
		switch {
		case head == "":
		case !nested:
			loose++
		default:
			if _, seen := counts[head]; !seen {
				order = append(order, head)
			}
			counts[head]++
		}
	}
	slices.Sort(order)
	out := make([]unit, 0, len(order)+1)
	for _, d := range order {
		out = append(out, unit{path: prefix + d, count: counts[d]})
	}
	if loose > 0 {
		out = append(out, unit{dir: strings.TrimSuffix(prefix, "/"), count: loose})
	}
	return out
}

// largest returns the index of the unit holding the most files, -1 when empty.
func largest(us []unit) int {
	best := -1
	for i, u := range us {
		if best < 0 || u.count > us[best].count {
			best = i
		}
	}
	return best
}

// surveyable drops hidden and dependency paths, plus the files stage 1 and the
// build agent already read — a codebase slice should not re-read them.
func surveyable(files []string) []string {
	return bulk.SliceFilterInPlace(func(f string) bool {
		if strings.HasPrefix(f, ".") || tools.IsSkippedDir(f) {
			return false
		}
		if strings.Contains(f, "/") {
			return true // the owned-elsewhere names only matter at the root
		}
		_, owned := ownedElsewhere[f]
		return !owned && !strings.HasPrefix(f, "README")
	}, files)
}

// leastLoaded returns the index of the smallest bucket.
func leastLoaded(load []int) int {
	return slices.Index(load, slices.Min(load))
}

// isFile reports whether path exists and is a regular file.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// repoFiles lists the repository's files relative to cwd, honouring .gitignore
// where git can answer so ignored trees never inflate a slice.
func repoFiles(cwd string) []string {
	if tools.IsGitRepo(cwd) {
		if out := gitLsFiles(cwd); len(out) > 0 {
			return out
		}
	}
	return walkFiles(cwd)
}

// gitLsFiles returns tracked and untracked non-ignored paths, or nil on failure.
func gitLsFiles(cwd string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	var out strings.Builder
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "ls-files", "-co", "--exclude-standard")
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return nil
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// walkFiles is the non-repository fallback: a bounded walk skipping VCS and
// dependency directories.
func walkFiles(cwd string) []string {
	var out []string
	_ = filepath.WalkDir(cwd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable subtree is skipped, not fatal
		}
		rel, rerr := filepath.Rel(cwd, path)
		if rerr != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || tools.IsSkippedDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if out = append(out, filepath.ToSlash(rel)); len(out) >= maxWalkFiles {
			return filepath.SkipAll
		}
		return nil
	})
	return out
}
