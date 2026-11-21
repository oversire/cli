package wire

// VendorRunArgs is the args of the app call vendor.run (app-call.json VendorRunArgs). Set Project or Account.
type VendorRunArgs struct {
	Project string   `json:"project,omitempty"`
	Account string   `json:"account,omitempty"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd,omitempty"`
}

// VendorRunOutput is the data of a vendor.run result (app-call.json VendorRunOutput).
type VendorRunOutput struct {
	Vendor    string `json:"vendor"`
	Account   string `json:"account"`
	Exit      int    `json:"exit"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timedOut,omitempty"`
	Ms        int64  `json:"ms"`
}

// VendorCLIRow is one row of the data of vendor.list (app-call.json VendorCLIRow).
type VendorCLIRow struct {
	Provider string   `json:"provider"`
	Vendor   string   `json:"vendor"`
	Binary   string   `json:"binary"`
	Path     string   `json:"path,omitempty"`
	Accounts []string `json:"accounts"`
	Projects []string `json:"projects"`
}
