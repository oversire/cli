package questions

import (
	"errors"
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
	server.WriteJSON(w, http.StatusOK, wire.DaemonQuestionsPage{Questions: c.Open()})
}

func (c *Channel) serveAnswer(w http.ResponseWriter, r *http.Request) {
	var a wire.QuestionAnswer
	err := server.ReadJSON(r, 1<<20, &a)
	if err != nil {
		server.WriteJSON(w, http.StatusBadRequest, wire.DaemonAnswerResult{Result: wire.AnswerError, Code: "bad_request", Message: err.Error()})
		return
	}
	res, err := c.Answer(r.Context(), r.PathValue("id"), a)
	switch {
	case errors.Is(err, ErrBadAnswer):
		server.WriteJSON(w, http.StatusBadRequest, wire.DaemonAnswerResult{Result: wire.AnswerError, Code: "bad_answer", Message: err.Error()})
	case err != nil:
		server.WriteJSON(w, http.StatusBadGateway, wire.DaemonAnswerResult{Result: wire.AnswerError, Code: CodeTerminal, Message: err.Error()})
	default:
		server.WriteJSON(w, http.StatusOK, res)
	}
}

func (c *Channel) serveGiven(w http.ResponseWriter, r *http.Request) {
	a, err := c.Given(r.Context(), r.PathValue("id"))
	if err != nil {
		server.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, struct {
		Answer *wire.QuestionAnswer `json:"answer"`
	}{a})
}
