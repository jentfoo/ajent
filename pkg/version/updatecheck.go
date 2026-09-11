package version

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/httputil"
)

const (
	// UpdateCacheFileName holds the latest-tag and last-notice state under
	// ~/.ajent/cache.
	UpdateCacheFileName = "remote-version.json"

	// remoteVersionTTL reuses a fetched tag within this window before asking GitHub again.
	remoteVersionTTL = 12 * time.Hour
	// noticeInterval is the minimum gap between update notices for one install.
	noticeInterval = 2 * time.Hour

	fetchTimeout = 5 * time.Second
)

var githubTagsURL = "https://api.github.com/repos/jentfoo/ajent/tags"

// UpdateCache is ~/.ajent/cache/remote-version.json.
type UpdateCache struct {
	Version   string `json:"version"`       // latest known tag, e.g. v0.1.5
	CheckedAt int64  `json:"checkedAtUnix"` // when Version was fetched (seconds)
	NoticedAt int64  `json:"noticedAtUnix"` // last time an update notice was shown (seconds)
}

// UpdateCheckOptions injects the seams CheckUpdateNotice needs to be tested.
type UpdateCheckOptions struct {
	Now   func() time.Time                          // defaults to time.Now
	Fetch func(ctx context.Context) (string, error) // latest remote tag; nil uses GitHub API
}

// checkTimeout bounds the whole startup notice lookup; the fetch inside it is
// bounded separately by fetchTimeout.
const checkTimeout = 10 * time.Second

// CheckForUpdate runs one bounded update-notice lookup and hands any notice to
// notify. Best effort: a failed or suppressed check reports nothing.
func CheckForUpdate(notify func(string)) {
	path, err := config.CachePath(UpdateCacheFileName)
	if err != nil {
		return // no home dir; nothing to cache or compare
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	msg, cerr := CheckUpdateNotice(ctx, path, UpdateCheckOptions{})
	if cerr != nil {
		return // offline or no tags: best-effort only
	}
	if msg != "" {
		notify(msg)
	}
}

// CheckUpdateNotice returns an update-available notice line or "" when none
// applies. It refreshes the cached latest tag at most once per 12h — fetching
// from GitHub otherwise — and only reports when the running build is not dev,
// older than the remote version, and no notice was shown in the last 2h.
func CheckUpdateNotice(ctx context.Context, cacheFile string, opts UpdateCheckOptions) (string, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	fetch := fetchLatestVersion
	if opts.Fetch != nil {
		fetch = opts.Fetch
	}

	var c UpdateCache
	loadUpdateCache(cacheFile, &c)

	// refresh a stale cached tag; a failed fetch keeps the old value and is not fatal.
	if now().Sub(time.Unix(c.CheckedAt, 0)) > remoteVersionTTL {
		latest, err := fetch(ctx)
		if err != nil {
			return "", fmt.Errorf("update check: %w", err)
		}
		c.Version = latest
		c.CheckedAt = now().Unix()
		_ = saveUpdateCache(cacheFile, c) // best effort; a failed write keeps the old cache
	}

	// only real builds are worth nagging about; dev is always behind by design.
	if Version == "" || Version == "dev" {
		return "", nil
	}
	current, ok := parseVersion(Version)
	if !ok {
		return "", nil // not a clean tag (dirty git describe), nothing to compare
	}
	remote, rok := parseVersion(c.Version)
	if !rok || !current.less(remote) {
		return "", nil // nothing newer to report
	}

	// at most one notice per install within the interval.
	if now().Sub(time.Unix(c.NoticedAt, 0)) < noticeInterval {
		return "", nil
	}
	c.NoticedAt = now().Unix()
	_ = saveUpdateCache(cacheFile, c) // best effort; a failed write only skips the next dedupe
	return fmt.Sprintf("update available: ajent %s → %s (run /update or --update)", Version, remote.String()), nil
}

// loadUpdateCache reads the update cache; any failure yields a zero value.
func loadUpdateCache(path string, c *UpdateCache) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, c)
}

// saveUpdateCache writes the update cache atomically.
func saveUpdateCache(path string, c UpdateCache) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, data, config.SecretPerm)
}

type ghTag struct {
	Name string `json:"name"`
}

// fetchLatestVersion returns the highest clean vX.Y.Z tag from GitHub.
func fetchLatestVersion(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	resp, err := httputil.Do(cctx, httputil.New(httputil.Options{}), httputil.Request{
		Method:    http.MethodGet,
		URL:       githubTagsURL,
		Name:      "update-check",
		UserAgent: UserAgent(),
	})
	if err != nil {
		return "", fmt.Errorf("github get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var tags []ghTag
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	max, ok := semver{}, false
	for _, t := range tags {
		v, vok := parseVersion(t.Name)
		if !vok {
			continue
		}
		if !ok || max.less(v) {
			max, ok = v, true
		}
	}
	if !ok {
		return "", errors.New("no clean vX.Y.Z tags returned")
	}
	return max.String(), nil
}

// cleanTag matches a strict release tag with no pre-release suffix.
var cleanTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

type semver struct{ major, minor, patch int }

func (v semver) String() string {
	return fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch)
}

func (v semver) less(o semver) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

// parseVersion parses a strict clean tag, rejecting any pre-release suffix.
func parseVersion(s string) (semver, bool) {
	m := cleanTag.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return semver{}, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return semver{major: major, minor: minor, patch: patch}, true
}
