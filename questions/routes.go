package questions

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// CodeTerminal: the read of the screen or the send of a key failed.
const CodeTerminal = "terminal_failed"

// Routes gives the question routes of the local-hub API.
func (c *Channel) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /v1/questions", Access: server.Member, Handle: c.serveOpen},
		{Pattern: "POST /v1/questions/{id}/answer", Access: server.Member, Handle: c.serveAnswer},
		{Pattern: "GET /v1/questions/{id}/given", Access: server.Member, Handle: c.serveGiven},
	}
}

func (c *Channel) serveOpen(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, wire.DaemonQuestionsPage{Questions: c.Open()})
}

func (c *Channel) serveAnswer(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var a wire.QuestionAnswer
	if err == nil {
		err = wire.Decode(body, &a)
	}
	if err != nil {
		reply(w, http.StatusBadRequest, wire.DaemonAnswerResult{Result: wire.AnswerError, Code: "bad_request", Message: err.Error()})
		return
	}
	res, err := c.Answer(r.Context(), r.PathValue("id"), a)
	switch {
	case errors.Is(err, ErrBadAnswer):
		reply(w, http.StatusBadRequest, wire.DaemonAnswerResult{Result: wire.AnswerError, Code: "bad_answer", Message: err.Error()})
	case err != nil:
		reply(w, http.StatusBadGateway, wire.DaemonAnswerResult{Result: wire.AnswerError, Code: CodeTerminal, Message: err.Error()})
	default:
		reply(w, http.StatusOK, res)
	}
}

func (c *Channel) serveGiven(w http.ResponseWriter, r *http.Request) {
	a, err := c.Given(r.Context(), r.PathValue("id"))
	if err != nil {
		reply(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	reply(w, http.StatusOK, struct {
		Answer *wire.QuestionAnswer `json:"answer"`
	}{a})
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
