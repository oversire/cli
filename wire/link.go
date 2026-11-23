package wire

// OpenSource is where an open came from (link.json OpenRoute.source).
type OpenSource string

const (
	OpenWeb OpenSource = "web"
	OpenCLI OpenSource = "cli"
)

// OpenRoute is one open of a route in the app.
type OpenRoute struct {
	ID     string     `json:"id"`
	Path   string     `json:"path"`
	Focus  string     `json:"focus,omitempty"`
	Source OpenSource `json:"source"`
	Daemon string     `json:"daemon"`
	Agent  string     `json:"agent,omitempty"`
}

const FrameOpenRoute = "openRoute"

// OpenRouteFrame is the frame on the WebSocket of the app that opens a route.
type OpenRouteFrame struct {
	Type string    `json:"type"`
	Open OpenRoute `json:"open"`
}

// AppPresence is the body of POST /v1/presence: seconds since the last user input on the Mac of the app.
type AppPresence struct {
	Idle int `json:"idle"`
}

// OpenRequest is the body of POST /v1/open.
type OpenRequest struct {
	Path  string `json:"path"`
	Focus string `json:"focus,omitempty"`
	On    string `json:"on,omitempty"`
	Agent string `json:"agent,omitempty"`
}

// OpenHow tells how the daemon chose the app.
type OpenHow string

const (
	OpenByClick OpenHow = "click"
	OpenByInput OpenHow = "input"
	OpenOn      OpenHow = "on"
)

// OpenResult is the answer of POST /v1/open.
type OpenResult struct {
	ID      string  `json:"id"`
	Machine string  `json:"machine"`
	How     OpenHow `json:"how"`
	Idle    *int    `json:"idle,omitempty"`
	Group   string  `json:"group,omitempty"`
}
