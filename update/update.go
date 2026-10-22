// Package update replaces the CLI with the latest GitHub release.
package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	// LatestURL redirects to releases/tag/<tag>. It is not an API call, so the limit of 60 calls in one hour does not apply.
	LatestURL = "https://github.com/oversire/cli/releases/latest"
	TeamID    = "Q56WK6TB88"
	maxSize   = 200 << 20
)

// Doer sends one HTTP request.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Runner runs one child process.
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

type Updater struct {
	http    Doer
	run     Runner
	api     string
	version string
	goos    string
	goarch  string
	exe     string
}

// NewUpdater replaces exe, the path of the running binary.
func NewUpdater(http Doer, run Runner, api, version, goos, goarch, exe string) *Updater {
	return &Updater{http: http, run: run, api: api, version: version, goos: goos, goarch: goarch, exe: exe}
}

// Result is one check. Installed is empty when this build is the latest.
type Result struct {
	Latest    string
	Installed string
}

// ErrDev: a dev build does not update.
var ErrDev = errors.New("update: a dev build does not update")

// Check installs a newer release. Latest is set also when the install fails.
func (u *Updater) Check(ctx context.Context) (Result, error) {
	if u.version == "dev" || u.version == "" {
		return Result{}, ErrDev
	}
	tag, download, err := u.latest(ctx)
	if err != nil {
		return Result{}, err
	}
	version := strings.TrimPrefix(tag, "v")
	r := Result{Latest: version}
	if !Newer(tag, u.version) {
		return r, nil
	}
	name := ArchiveName(version, u.goos, u.goarch)
	sums, err := u.get(ctx, download+"checksums.txt")
	if err != nil {
		return r, err
	}
	archive, err := u.get(ctx, download+name)
	if err != nil {
		return r, fmt.Errorf("update: release %s has no %s: %w", tag, name, err)
	}
	if err := VerifyChecksum(sums, name, archive); err != nil {
		return r, err
	}
	bin, err := extract(archive, u.binaryName(), u.goos == "windows")
	if err != nil {
		return r, err
	}
	if err := u.replace(ctx, bin); err != nil {
		return r, err
	}
	r.Installed = version
	return r, nil
}

func (u *Updater) binaryName() string {
	if u.goos == "windows" {
		return "oversire.exe"
	}
	return "oversire"
}

// latest follows the redirect of the latest URL to .../releases/tag/<tag>. download is the folder of the release files.
func (u *Updater) latest(ctx context.Context) (tag, download string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.api, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "oversire-cli/"+u.version)
	resp, err := u.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("update: cannot reach GitHub: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request == nil {
		return "", "", fmt.Errorf("update: GitHub answered %s for %s. Try again later", resp.Status, u.api)
	}
	final := resp.Request.URL.String()
	base, tag, ok := strings.Cut(final, "/releases/tag/")
	if !ok || tag == "" || strings.Contains(tag, "/") {
		return "", "", fmt.Errorf("update: %s gave no release tag (%s)", u.api, final)
	}
	return tag, base + "/releases/download/" + tag + "/", nil
}

func (u *Updater) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "oversire-cli/"+u.version)
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxSize))
}

// replace writes the new binary next to the old one, checks it and renames it into place.
func (u *Updater) replace(ctx context.Context, bin []byte) error {
	next := u.exe + ".new"
	if err := os.WriteFile(next, bin, 0o755); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("update: no write access to %s. Give your user write access to it, or remove %s and install the CLI again from the OverSire app", filepath.Dir(u.exe), u.exe)
		}
		return fmt.Errorf("update: %w", err)
	}
	if u.goos == "darwin" {
		if err := SignedByTeam(ctx, u.run, next); err != nil {
			_ = os.Remove(next)
			return err
		}
	}
	if u.goos == "windows" {
		// Windows cannot rename over a running binary, but it can rename the running binary.
		old := u.exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(u.exe, old); err != nil {
			return fmt.Errorf("update: %w", err)
		}
	}
	if err := os.Rename(next, u.exe); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

// SignedByTeam checks the code signature of file and its Team ID (macOS).
func SignedByTeam(ctx context.Context, run Runner, file string) error {
	if _, stderr, err := run.Run(ctx, "/usr/bin/codesign", []string{"--verify", "--strict", file}, nil); err != nil {
		return fmt.Errorf("update: the signature is not valid: %s", strings.TrimSpace(string(stderr)))
	}
	_, stderr, err := run.Run(ctx, "/usr/bin/codesign", []string{"-dv", file}, nil)
	if err != nil || !slices.Contains(strings.Split(string(stderr), "\n"), "TeamIdentifier="+TeamID) {
		return fmt.Errorf("update: the binary is not signed by team %s", TeamID)
	}
	return nil
}

// ArchiveName is the goreleaser archive of one platform.
func ArchiveName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("oversire_%s_%s_%s%s", version, goos, goarch, ext)
}

// VerifyChecksum checks data against the sha256 line of name in checksums.txt.
func VerifyChecksum(sums []byte, name string, data []byte) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	s := bufio.NewScanner(bytes.NewReader(sums))
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if !strings.EqualFold(f[0], got) {
				return fmt.Errorf("update: the checksum of %s does not agree", name)
			}
			return nil
		}
	}
	return fmt.Errorf("update: checksums.txt has no %s", name)
}

func extract(archive []byte, name string, isZip bool) ([]byte, error) {
	if isZip {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("update: %w", err)
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == name {
				r, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer r.Close()
				return io.ReadAll(io.LimitReader(r, maxSize))
			}
		}
		return nil, fmt.Errorf("update: the archive has no %s", name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("update: the archive has no %s", name)
		}
		if err != nil {
			return nil, fmt.Errorf("update: %w", err)
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == name {
			return io.ReadAll(io.LimitReader(tr, maxSize))
		}
	}
}

// Newer tells if latest is a higher version than current. A version that does not parse is never newer.
func Newer(latest, current string) bool {
	l, lpre, ok1 := parse(latest)
	c, cpre, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range 3 {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return cpre != "" && (lpre == "" || lpre > cpre)
}

func parse(v string) ([3]int, string, bool) {
	var n [3]int
	v = strings.TrimPrefix(v, "v")
	v, pre, _ := strings.Cut(v, "-")
	v, _, _ = strings.Cut(v, "+")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return n, "", false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, "", false
		}
		n[i] = x
	}
	return n, pre, true
}
