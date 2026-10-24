package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// The limits of POST /v1/files.
const (
	MaxFiles     = 20
	MaxFileBytes = 25 << 20 // all files of one call together
	filesMaxAge  = 7 * 24 * time.Hour
)

// API serves the runtimes of the registry on the local API.
type API struct {
	Log *slog.Logger

	reg   *Registry
	files string // the folder of POST /v1/files
	now   func() time.Time

	mu      sync.Mutex
	failing map[string]string // the last workspaces error of each runtime
}

// NewAPI gives the routes of reg. POST /v1/files writes into a new folder in files for each call.
func NewAPI(reg *Registry, files string, now func() time.Time) *API {
	return &API{Log: slog.Default(), reg: reg, files: files, now: now, failing: map[string]string{}}
}

func (a *API) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /v1/runtimes", Access: server.Member, Handle: a.runtimes},
		{Pattern: "POST /v1/runtimes/{id}/retry", Access: server.Member, Handle: a.retry},
		{Pattern: "POST /v1/runtimes/{id}/pair", Access: server.Member, Handle: a.pair},
		{Pattern: "DELETE /v1/runtimes/{id}/pair", Access: server.Member, Handle: a.unpair},
		{Pattern: "GET /v1/repos", Access: server.Member, Handle: a.repos},
		{Pattern: "GET /v1/workspaces", Access: server.Member, Handle: a.workspaces},
		{Pattern: "DELETE /v1/workspaces/{id}", Access: server.Member, Handle: a.removeWorkspace},
		{Pattern: "POST /v1/files", Access: server.Member, Handle: a.writeFiles},
		// Only the user at this machine sees the runtime UI.
		{Pattern: "POST /v1/reveal", Access: server.LoopbackOnly, Handle: a.reveal},
	}
}

func (a *API) runtimes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, wire.DaemonRuntimesPage{Runtimes: a.reg.Status(r.Context())})
}

func (a *API) retry(w http.ResponseWriter, r *http.Request) {
	d, err := a.reg.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, status(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *API) pair(w http.ResponseWriter, r *http.Request) {
	var p wire.RuntimePair
	if err := decode(r, 256<<10, &p); err != nil || strings.TrimSpace(p.Link) == "" {
		writeError(w, http.StatusBadRequest, "The body needs a pairing link.")
		return
	}
	d, err := a.reg.Pair(r.Context(), r.PathValue("id"), p.Link)
	if err != nil {
		code := status(err)
		if errors.As(err, new(ErrPair)) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err.Error())
		return
	}
	a.Log.Info("runtime paired", "runtime", d.ID)
	writeJSON(w, http.StatusOK, d)
}

func (a *API) unpair(w http.ResponseWriter, r *http.Request) {
	d, err := a.reg.Unpair(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, status(err), err.Error())
		return
	}
	a.Log.Info("runtime unpaired", "runtime", d.ID)
	writeJSON(w, http.StatusOK, d)
}

func (a *API) repos(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.reg.Repos(r.Context()))
}

func (a *API) workspaces(w http.ResponseWriter, r *http.Request) {
	page := a.reg.Workspaces(r.Context())
	a.logErrors(page.Errors)
	writeJSON(w, http.StatusOK, page)
}

// logErrors writes a runtime error of the workspaces page when it is new, and a line when the runtime works again.
// The app asks for the page often, so the same error again writes nothing.
func (a *API) logErrors(errs map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, e := range errs {
		if a.failing[id] != e {
			a.Log.Warn("runtime workspaces", "runtime", id, "err", e)
		}
	}
	for id := range a.failing {
		if _, ok := errs[id]; !ok {
			a.Log.Info("runtime workspaces work again", "runtime", id)
		}
	}
	a.failing = maps.Clone(errs)
	if a.failing == nil {
		a.failing = map[string]string{}
	}
}

func (a *API) removeWorkspace(w http.ResponseWriter, r *http.Request) {
	var rm wire.WorkspaceRemove
	if err := decode(r, 1<<16, &rm); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := a.reg.RemoveWorkspace(r.Context(), r.PathValue("id"), rm)
	if err != nil {
		writeError(w, status(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) reveal(w http.ResponseWriter, r *http.Request) {
	var s wire.SessionRef
	if err := decode(r, 1<<16, &s); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.reg.Reveal(r.Context(), s); err != nil {
		writeError(w, status(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) writeFiles(w http.ResponseWriter, r *http.Request) {
	var fs wire.WorkspaceFiles
	// base64 makes the body 4/3 of the data.
	if err := decode(r, MaxFileBytes/3*4+1<<16, &fs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := checkFiles(fs.Files); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.prune()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	dir := filepath.Join(a.files, hex.EncodeToString(b))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := wire.WorkspaceFilesResult{Paths: []string{}}
	for _, f := range fs.Files {
		p := filepath.Join(dir, f.Name)
		if err := os.WriteFile(p, f.Data, 0o600); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		res.Paths = append(res.Paths, p)
	}
	writeJSON(w, http.StatusOK, res)
}

// checkFiles refuses a count or size over the limits, and a name that is not a plain file name.
func checkFiles(files []wire.WorkspaceFile) error {
	if len(files) == 0 || len(files) > MaxFiles {
		return fmt.Errorf("send 1 to %d files", MaxFiles)
	}
	total := 0
	seen := map[string]bool{}
	for _, f := range files {
		n := f.Name
		if n == "" || len(n) > 255 || strings.HasPrefix(n, ".") || strings.ContainsAny(n, "/\\\x00") {
			return fmt.Errorf("bad file name %q", n)
		}
		if seen[n] {
			return fmt.Errorf("two files have the name %q", n)
		}
		seen[n] = true
		total += len(f.Data)
	}
	if total > MaxFileBytes {
		return fmt.Errorf("the files are larger than %d MB", MaxFileBytes>>20)
	}
	return nil
}

// prune removes the folders of calls older than filesMaxAge.
func (a *API) prune() {
	entries, err := os.ReadDir(a.files)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || a.now().Sub(info.ModTime()) < filesMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(a.files, e.Name()))
	}
}

func decode(r *http.Request, limit int64, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return errors.New("the body is too large")
	}
	return wire.Decode(body, v)
}

func status(err error) int {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoRuntime):
		return http.StatusNotFound
	case errors.Is(err, ErrUnsupported):
		return http.StatusNotImplemented
	}
	return http.StatusBadGateway
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
