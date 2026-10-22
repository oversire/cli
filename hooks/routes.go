package hooks

import (
	"encoding/json"
	"net/http"

	"github.com/oversire/cli/server"
)

type harnessesBody struct {
	Harnesses []string `json:"harnesses"`
}

// Routes let the app on this machine read, install and remove the hooks after the user agrees. Loopback only.
func (i *Installer) Routes() []server.Route {
	reply := func(w http.ResponseWriter, v any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	names := func(r *http.Request) ([]string, error) {
		var b harnessesBody
		if r.ContentLength == 0 {
			return nil, nil
		}
		err := json.NewDecoder(r.Body).Decode(&b)
		return b.Harnesses, err
	}
	return []server.Route{
		{Pattern: "GET /v1/hooks/install", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			reply(w, i.Status(), nil)
		}},
		{Pattern: "POST /v1/hooks/install", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			n, err := names(r)
			if err != nil {
				reply(w, nil, err)
				return
			}
			st, err := i.Install(n...)
			reply(w, st, err)
		}},
		{Pattern: "DELETE /v1/hooks/install", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			n, err := names(r)
			if err == nil {
				err = i.Remove(n...)
			}
			reply(w, i.Status(), err)
		}},
	}
}
