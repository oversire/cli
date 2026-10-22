package wire

import "encoding/json"

// StoreSchema is the x-oversire-schema number of store.json.
const StoreSchema = 2

// StoreSchemasAccepted are the store schema numbers that a store accepts: this one and the one before.
var StoreSchemasAccepted = []int{1, 2}

// StoreOpKind is put or delete.
type StoreOpKind string

const (
	StorePut    StoreOpKind = "put"
	StoreDelete StoreOpKind = "delete"
)

// StoreOp is one write to a synced collection. Last write wins for each collection and id, by HLC.
type StoreOp struct {
	Schema     int             `json:"schema"`
	Collection string          `json:"collection"`
	ID         string          `json:"id"`
	Op         StoreOpKind     `json:"op"`
	Record     json.RawMessage `json:"record,omitempty"` // missing for a delete
	HLC        string          `json:"hlc"`
	Device     string          `json:"device"`
}

// StoreOps is the body of POST /v1/store/ops.
type StoreOps struct {
	Ops []StoreOp `json:"ops"`
}

// StoreApplyResult is the answer of POST /v1/store/ops: the ops that won, and the head after them.
type StoreApplyResult struct {
	Accepted []StoreOp `json:"accepted"`
	Head     int       `json:"head"`
}

// StorePull is a page of GET /v1/store/ops: the newest op of each record that changed after the cursor of the request.
// With reset, the page holds all records, and a record that is not in it is gone.
type StorePull struct {
	Ops    []StoreOp `json:"ops"`
	Cursor int       `json:"cursor"`
	More   bool      `json:"more"`
	Reset  bool      `json:"reset"`
}
