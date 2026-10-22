package questions

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Searcher finds the transcript files of Claude sessions in a folder by words (orca search).
type Searcher interface {
	Search(ctx context.Context, cwd, words string) ([]string, error)
}

// Files reads the Claude transcripts under a projects folder, normally ~/.claude/projects. It implements Transcripts.
type Files struct {
	projects string
	search   Searcher
}

// NewFiles: search can be nil. Then Find gives only the newest file of the folder.
func NewFiles(projects string, search Searcher) *Files {
	return &Files{projects: projects, search: search}
}

// Tail gives the last n bytes of the file. The first line can be cut; the readers skip it.
func (f *Files) Tail(path string, n int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if off := st.Size() - int64(n); off > 0 {
		if _, err := file.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(file, int64(n)))
}

// Find gives the newest transcript in the project folder of cwd, then the files where the search finds the words.
// Many sessions can run in one folder, so the newest file can be of another session.
func (f *Files) Find(ctx context.Context, cwd, words string) ([]string, error) {
	var out []string
	if newest := f.newest(cwd); newest != "" {
		out = append(out, newest)
	}
	query := strings.Join(firstN(strings.FieldsFunc(words, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }), 10), " ")
	if f.search == nil || query == "" {
		return out, nil
	}
	hits, err := f.search.Search(ctx, cwd, query)
	return append(out, hits...), err
}

func (f *Files) newest(cwd string) string {
	paths, _ := filepath.Glob(filepath.Join(f.projects, ProjectDir(cwd), "*.jsonl"))
	best, at := "", int64(0)
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.ModTime().UnixNano() > at {
			best, at = p, st.ModTime().UnixNano()
		}
	}
	return best
}

// ProjectDir is the folder name that Claude Code gives to the transcripts of cwd: each character that is not a letter, a digit or "-" becomes "-".
func ProjectDir(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-') {
			return r
		}
		return '-'
	}, cwd)
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
