package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.0", "0.1.0", true},
		{"v0.1.10", "0.1.9", true},
		{"v1.0.0", "0.9.9", true},
		{"v0.1.0", "0.1.0", false},
		{"v0.1.0", "0.2.0", false},
		{"v0.1.0", "0.1.0-rc1", true},
		{"v0.1.0-rc2", "0.1.0-rc1", true},
		{"v0.1.0-rc1", "0.1.0", false},
		{"v0.2.0", "dev", false},
		{"nonsense", "0.1.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}

func TestArchiveName(t *testing.T) {
	if got := ArchiveName("0.2.0", "darwin", "arm64"); got != "oversire_0.2.0_darwin_arm64.tar.gz" {
		t.Fatal(got)
	}
	if got := ArchiveName("0.2.0", "windows", "amd64"); got != "oversire_0.2.0_windows_amd64.zip" {
		t.Fatal(got)
	}
}

func targz(t *testing.T, name string, data []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{"README.md", []byte("readme")}, {name, data}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg})
		tw.Write(f.data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipped(t *testing.T, name string, data []byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(name)
	w.Write(data)
	zw.Close()
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// feed serves the latest redirect and the release files from memory.
type feed struct {
	files     map[string][]byte
	redirects map[string]string
	gets      []string
}

func (f *feed) Do(r *http.Request) (*http.Response, error) {
	f.gets = append(f.gets, r.URL.String())
	if to, ok := f.redirects[r.URL.String()]; ok {
		final, _ := http.NewRequest(r.Method, to, nil)
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader("")), Request: final}, nil
	}
	b, ok := f.files[r.URL.String()]
	if !ok {
		return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(b)), Request: r}, nil
}

const latestURL = "https://gh/o/cli/releases/latest"

func newFeed(tag, archiveName string, archive []byte, sums string) *feed {
	dl := "https://gh/o/cli/releases/download/" + tag + "/"
	return &feed{
		redirects: map[string]string{latestURL: "https://gh/o/cli/releases/tag/" + tag},
		files:     map[string][]byte{dl + archiveName: archive, dl + "checksums.txt": []byte(sums)},
	}
}

type codesign struct {
	team  string
	valid bool
	calls [][]string
}

func (c *codesign) Run(_ context.Context, exe string, args []string, _ []byte) ([]byte, []byte, error) {
	c.calls = append(c.calls, append([]string{exe}, args...))
	if args[0] == "--verify" {
		if !c.valid {
			return nil, []byte("invalid signature"), errors.New("exit status 1")
		}
		return nil, nil, nil
	}
	return nil, []byte("Executable=/x\nIdentifier=oversire\nTeamIdentifier=" + c.team + "\n"), nil
}

func exeFile(t *testing.T) string {
	exe := filepath.Join(t.TempDir(), "oversire")
	os.WriteFile(exe, []byte("old"), 0o755)
	return exe
}

func TestUpdateReplacesTheBinary(t *testing.T) {
	name := ArchiveName("0.2.0", "darwin", "arm64")
	archive := targz(t, "oversire", []byte("new"))
	f := newFeed("v0.2.0", name, archive, sha(archive)+"  "+name+"\n")
	cs := &codesign{team: TeamID, valid: true}
	exe := exeFile(t)
	got, err := NewUpdater(f, cs, latestURL, "0.1.0", "darwin", "arm64", exe).Check(context.Background())
	if err != nil || got != "0.2.0" {
		t.Fatal(got, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("binary %q", b)
	}
	if len(cs.calls) != 2 || cs.calls[0][2] != "--strict" {
		t.Fatalf("codesign calls %q", cs.calls)
	}
}

func TestUpdateWindowsZip(t *testing.T) {
	name := ArchiveName("0.2.0", "windows", "amd64")
	archive := zipped(t, "oversire.exe", []byte("new"))
	f := newFeed("v0.2.0", name, archive, sha(archive)+"  "+name+"\n")
	exe := exeFile(t)
	if _, err := NewUpdater(f, &codesign{}, latestURL, "0.1.0", "windows", "amd64", exe).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("binary %q", b)
	}
}

func TestUpdateRefusals(t *testing.T) {
	name := ArchiveName("0.2.0", "darwin", "arm64")
	archive := targz(t, "oversire", []byte("new"))
	good := sha(archive) + "  " + name + "\n"
	cases := []struct {
		label, version, sums, want string
		cs                         *codesign
	}{
		{"bad checksum", "0.1.0", strings.Repeat("0", 64) + "  " + name + "\n", "checksum", &codesign{team: TeamID, valid: true}},
		{"no checksum line", "0.1.0", "abc  other.tar.gz\n", "has no", &codesign{team: TeamID, valid: true}},
		{"bad signature", "0.1.0", good, "signature", &codesign{team: TeamID}},
		{"other team", "0.1.0", good, "team", &codesign{team: "XXXXXXXXXX", valid: true}},
	}
	for _, c := range cases {
		exe := exeFile(t)
		f := newFeed("v0.2.0", name, archive, c.sums)
		_, err := NewUpdater(f, c.cs, latestURL, c.version, "darwin", "arm64", exe).Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v", c.label, err)
		}
		if b, _ := os.ReadFile(exe); string(b) != "old" {
			t.Errorf("%s: the binary changed", c.label)
		}
		if _, err := os.Stat(exe + ".new"); err == nil {
			t.Errorf("%s: a .new file stays", c.label)
		}
	}
}

func TestNoUpdate(t *testing.T) {
	name := ArchiveName("0.1.0", "linux", "amd64")
	for _, version := range []string{"0.1.0", "dev"} {
		f := newFeed("v0.1.0", name, nil, "")
		got, err := NewUpdater(f, &codesign{}, latestURL, version, "linux", "amd64", exeFile(t)).Check(context.Background())
		if err != nil || got != "" {
			t.Fatal(version, got, err)
		}
		if len(f.gets) > 1 || (version == "dev" && len(f.gets) != 0) {
			t.Fatalf("%s: gets %q", version, f.gets)
		}
	}
}

func TestLatestWithoutTag(t *testing.T) {
	f := &feed{redirects: map[string]string{latestURL: "https://gh/o/cli/releases"}}
	_, err := NewUpdater(f, &codesign{}, latestURL, "0.1.0", "linux", "amd64", exeFile(t)).Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no release tag") {
		t.Fatal(err)
	}
}
