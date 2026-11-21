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
// Path is the binary that vendor.run runs. An app before OVERSIRE-580 sends no State.
type VendorCLIRow struct {
	Provider   string   `json:"provider"`
	Vendor     string   `json:"vendor"`
	Binary     string   `json:"binary"`
	Path       string   `json:"path,omitempty"`
	Version    string   `json:"version,omitempty"`
	MinVersion string   `json:"minVersion,omitempty"`
	Source     string   `json:"source,omitempty"`
	State      string   `json:"state,omitempty"`
	Accounts   []string `json:"accounts"`
	Projects   []string `json:"projects"`
	OwnVersion string   `json:"ownVersion,omitempty"`
	Job        string   `json:"job,omitempty"`
}

// VendorActionArgs is the args of the app calls vendor.install and vendor.remove (app-call.json VendorActionArgs).
type VendorActionArgs struct {
	Vendor string `json:"vendor"`
}

// VendorChange is the data of a vendor.install or vendor.remove result (app-call.json VendorChange).
type VendorChange struct {
	Vendor   string `json:"vendor"`
	Binary   string `json:"binary"`
	Action   string `json:"action"`
	Version  string `json:"version,omitempty"`
	Previous string `json:"previous,omitempty"`
	Path     string `json:"path,omitempty"`
	Release  string `json:"release,omitempty"`
}

// Problem tells why the app does not run the binary, or "" for state ok and for no state.
// The texts are those of VendorBinary.problem in the app.
func (r VendorCLIRow) Problem() string {
	hint := "Run `oversire vendor install " + r.Provider + "`."
	if r.Binary == "gh" {
		hint = "Install gh with Homebrew: `brew install gh`."
	}
	switch r.State {
	case "missing":
		return r.Binary + " is not installed. " + hint
	case "old":
		if r.Version == "" {
			return r.Binary + " at " + r.Path + " gives no version. OverSire needs " + r.MinVersion + " or newer. " + hint
		}
		return r.Binary + " " + r.Version + " is older than " + r.MinVersion + ". " + hint
	}
	return ""
}

// VendorSkillArgs is the args of the app call vendor.skill (app-call.json VendorSkillArgs).
type VendorSkillArgs struct {
	Vendor string `json:"vendor"`
}

// VendorSkillOutput is the data of a vendor.skill result (app-call.json VendorSkillOutput).
// Reference is skill, help or notInstalled.
type VendorSkillOutput struct {
	Vendor    string `json:"vendor"`
	Binary    string `json:"binary"`
	Path      string `json:"path,omitempty"`
	Reference string `json:"reference"`
	Markdown  string `json:"markdown"`
}
