package wire

import (
	"encoding/json"
	"time"
)

// The mirror feed of the home daemon. Contract: mirror.json.

type MirrorKind string

const (
	MirrorProject  MirrorKind = "project"
	MirrorItem     MirrorKind = "item"
	MirrorRuns     MirrorKind = "runs"
	MirrorPRs      MirrorKind = "prs"
	MirrorActivity MirrorKind = "activity"
	MirrorSettings MirrorKind = "settings"
	MirrorCatalog  MirrorKind = "catalog"
)

// Valid tells if k is a kind of the contract.
func (k MirrorKind) Valid() bool {
	switch k {
	case MirrorProject, MirrorItem, MirrorRuns, MirrorPRs, MirrorActivity, MirrorSettings, MirrorCatalog:
		return true
	}
	return false
}

// MirrorDoc is one doc of the feed. The daemon does not read Body. The source does not set Hash and Seq.
type MirrorDoc struct {
	Kind    MirrorKind      `json:"kind"`
	ID      string          `json:"id"`
	V       int             `json:"v"`
	Hash    string          `json:"hash"`
	Seq     int64           `json:"seq"`
	At      time.Time       `json:"at"`
	Deleted bool            `json:"deleted,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
}

// MirrorPut is the body of PUT /v1/mirror/docs. Source is the app device that holds the automations lease.
type MirrorPut struct {
	Epoch      string      `json:"epoch"`
	Source     string      `json:"source"`
	SourceName string      `json:"sourceName"`
	AppVersion string      `json:"appVersion"`
	Ready      bool        `json:"ready,omitempty"`
	Docs       []MirrorDoc `json:"docs"`
}

type MirrorPutResult struct {
	Head    MirrorHead `json:"head"`
	Applied int        `json:"applied"`
	Same    int        `json:"same"`
}

type MirrorHead struct {
	Epoch         string         `json:"epoch"`
	Head          int64          `json:"head"`
	Floor         int64          `json:"floor"`
	Ready         bool           `json:"ready"`
	Source        string         `json:"source,omitempty"`
	SourceName    string         `json:"sourceName,omitempty"`
	SourceMachine string         `json:"sourceMachine,omitempty"`
	AppVersion    string         `json:"appVersion,omitempty"`
	PublishedAt   *time.Time     `json:"publishedAt,omitempty"`
	SourceOnline  bool           `json:"sourceOnline,omitempty"`
	Readers       []MirrorReader `json:"readers,omitempty"`
}

type MirrorReader struct {
	Machine string    `json:"machine"`
	Name    string    `json:"name"`
	Device  string    `json:"device,omitempty"`
	Head    int64     `json:"head"`
	SeenAt  time.Time `json:"seenAt"`
}

// MirrorPage is the answer of GET /v1/mirror/docs. Reset: the reader drops its docs and reads from 0.
type MirrorPage struct {
	Epoch string      `json:"epoch"`
	Docs  []MirrorDoc `json:"docs"`
	Next  int64       `json:"next"`
	More  bool        `json:"more"`
	Reset bool        `json:"reset"`
}

type MirrorIndexEntry struct {
	Kind MirrorKind `json:"kind"`
	ID   string     `json:"id"`
	Hash string     `json:"hash"`
}

type MirrorIndex struct {
	Epoch string             `json:"epoch"`
	Head  int64              `json:"head"`
	Docs  []MirrorIndexEntry `json:"docs"`
}

// FrameMirror is the type of the WebSocket frame that tells the clients to read the feed.
const FrameMirror = "mirror"

type MirrorFrame struct {
	Type  string `json:"type"`
	Head  int64  `json:"head"`
	Epoch string `json:"epoch"`
}
