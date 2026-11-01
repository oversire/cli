package wire

import "time"

// The review gate (review.json, OVERSIRE-365).

type ReviewKind string

const (
	ReviewCode ReviewKind = "code"
	ReviewPlan ReviewKind = "plan"
)

// ReviewState: ready is the turn of the user, waiting is the turn of the agent.
type ReviewState string

const (
	ReviewReadyState ReviewState = "ready"
	ReviewWaiting    ReviewState = "waiting"
	ReviewConflict   ReviewState = "conflict"
	ReviewMerged     ReviewState = "merged"
)

type MergeMode string

const (
	MergeFastForward MergeMode = "fastForward"
	MergeRebase      MergeMode = "rebase"
	MergeConflict    MergeMode = "conflict"
)

type ReviewSessionState string

const (
	ReviewSessionIdle    ReviewSessionState = "idle"
	ReviewSessionWorking ReviewSessionState = "working"
	ReviewSessionEnded   ReviewSessionState = "ended"
	ReviewSessionNone    ReviewSessionState = "none"
)

type ReviewSession struct {
	ID      string             `json:"id,omitempty"`
	State   ReviewSessionState `json:"state"`
	Harness string             `json:"harness,omitempty"`
	Profile string             `json:"profile,omitempty"`
	CanSend bool               `json:"canSend"`
}

type ReviewCommit struct {
	SHA     string   `json:"sha"`
	Subject string   `json:"subject"`
	Items   []string `json:"items,omitempty"`
}

// ReviewRepo is one repo of a group that has a worktree.
type ReviewRepo struct {
	Name          string         `json:"name"`
	Branch        string         `json:"branch"`
	Base          string         `json:"base"`
	Ahead         int            `json:"ahead"`
	Behind        int            `json:"behind"`
	Added         int            `json:"added"`
	Removed       int            `json:"removed"`
	Files         int            `json:"files"`
	Dirty         bool           `json:"dirty"`
	Merge         MergeMode      `json:"merge"`
	ConflictFiles []string       `json:"conflictFiles,omitempty"`
	Commits       []ReviewCommit `json:"commits,omitempty"`
}

// ReviewSummary is one row of Work › Review.
type ReviewSummary struct {
	Key        string         `json:"key"`
	Kind       ReviewKind     `json:"kind"`
	Title      string         `json:"title,omitempty"`
	Machine    string         `json:"machine"`
	State      ReviewState    `json:"state"`
	Round      int            `json:"round"`
	Pending    int            `json:"pending"`
	Unanswered int            `json:"unanswered"`
	Root       string         `json:"root"`
	Group      string         `json:"group,omitempty"`
	Repos      []ReviewRepo   `json:"repos"`
	Subitems   []string       `json:"subitems"`
	Session    *ReviewSession `json:"session,omitempty"`
	Note       string         `json:"note,omitempty"`
	ReadyAt    *time.Time     `json:"readyAt,omitempty"`
	UpdatedAt  time.Time      `json:"updatedAt"`
}

// NotPushed is a base branch with local commits that its upstream does not have.
type NotPushed struct {
	Root  string `json:"root"`
	Repo  string `json:"repo"`
	Base  string `json:"base"`
	Ahead int    `json:"ahead"`
}

// ReviewsPage is the answer of GET /v1/reviews.
type ReviewsPage struct {
	Reviews   []ReviewSummary `json:"reviews"`
	NotPushed []NotPushed     `json:"notPushed"`
}

// FileStatus: A, M, D or R.
type FileStatus string

const (
	FileAdded    FileStatus = "A"
	FileModified FileStatus = "M"
	FileDeleted  FileStatus = "D"
	FileRenamed  FileStatus = "R"
)

type ReviewFile struct {
	Repo      string     `json:"repo"`
	Path      string     `json:"path"`
	OldPath   string     `json:"oldPath,omitempty"`
	Status    FileStatus `json:"status"`
	Added     int        `json:"added"`
	Removed   int        `json:"removed"`
	Binary    bool       `json:"binary"`
	Untracked bool       `json:"untracked,omitempty"`
	Viewed    bool       `json:"viewed"`
}

type DiffLineKind string

const (
	DiffCtx DiffLineKind = "ctx"
	DiffAdd DiffLineKind = "add"
	DiffDel DiffLineKind = "del"
)

type DiffLine struct {
	Kind DiffLineKind `json:"kind"`
	Old  int          `json:"old,omitempty"`
	New  int          `json:"new,omitempty"`
	Text string       `json:"text"`
}

type DiffHunk struct {
	OldStart int        `json:"oldStart"`
	OldLines int        `json:"oldLines"`
	NewStart int        `json:"newStart"`
	NewLines int        `json:"newLines"`
	Header   string     `json:"header"`
	Lines    []DiffLine `json:"lines"`
}

// FileDiff is the answer of GET /v1/reviews/{key}/diff.
type FileDiff struct {
	Repo      string     `json:"repo"`
	Path      string     `json:"path"`
	OldPath   string     `json:"oldPath,omitempty"`
	Status    FileStatus `json:"status"`
	Binary    bool       `json:"binary"`
	Truncated bool       `json:"truncated"`
	Hunks     []DiffHunk `json:"hunks"`
}

// Anchor is a code anchor (Repo, Path, Line, From, Side) or a plan anchor (Block).
type Anchor struct {
	Repo  string `json:"repo,omitempty"`
	Path  string `json:"path,omitempty"`
	Line  int    `json:"line,omitempty"`
	From  int    `json:"from,omitempty"`
	Side  string `json:"side,omitempty"` // old or new; empty is new
	Block string `json:"block,omitempty"`
}

// IsPlan tells if the anchor is a plan block.
func (a Anchor) IsPlan() bool { return a.Block != "" }

type CommentState string

const (
	CommentPending   CommentState = "pending"
	CommentSubmitted CommentState = "submitted"
	CommentAnswered  CommentState = "answered"
)

type ReviewComment struct {
	ID         string       `json:"id"`
	Key        string       `json:"key"`
	Round      int          `json:"round"`
	State      CommentState `json:"state"`
	Anchor     Anchor       `json:"anchor"`
	Excerpt    string       `json:"excerpt,omitempty"`
	Text       string       `json:"text"`
	Reply      string       `json:"reply,omitempty"`
	CreatedAt  time.Time    `json:"createdAt"`
	UpdatedAt  *time.Time   `json:"updatedAt,omitempty"`
	AnsweredAt *time.Time   `json:"answeredAt,omitempty"`
}

type ReviewVerdict string

const (
	VerdictRequestChanges ReviewVerdict = "requestChanges"
	VerdictApprove        ReviewVerdict = "approve"
)

type Delivery string

const (
	DeliverySent       Delivery = "sent"
	DeliveryQueued     Delivery = "queued"
	DeliveryNewSession Delivery = "newSession"
	DeliveryNone       Delivery = "none"
)

type ReviewRound struct {
	N           int           `json:"n"`
	Verdict     ReviewVerdict `json:"verdict"`
	Summary     string        `json:"summary,omitempty"`
	Count       int           `json:"count"`
	SubmittedAt time.Time     `json:"submittedAt"`
	Delivery    Delivery      `json:"delivery"`
	Session     string        `json:"session,omitempty"`
	DeliveredAt *time.Time    `json:"deliveredAt,omitempty"`
	Error       string        `json:"error,omitempty"`
	AnsweredAt  *time.Time    `json:"answeredAt,omitempty"`
}

type PlanBlock struct {
	ID       string `json:"id"`
	Heading  string `json:"heading"`
	Markdown string `json:"markdown"`
}

type PlanSubitem struct {
	Key   string `json:"key,omitempty"`
	Title string `json:"title"`
}

// PlanDoc is the plan of a Plan session, split at each ## heading.
type PlanDoc struct {
	Key      string        `json:"key"`
	Version  int           `json:"version"`
	Title    string        `json:"title,omitempty"`
	Root     string        `json:"root,omitempty"`
	Blocks   []PlanBlock   `json:"blocks"`
	Subitems []PlanSubitem `json:"subitems,omitempty"`
}

// ReviewDetail is the answer of GET /v1/reviews/{key}.
type ReviewDetail struct {
	Summary  ReviewSummary   `json:"summary"`
	Files    []ReviewFile    `json:"files"`
	Comments []ReviewComment `json:"comments"`
	Rounds   []ReviewRound   `json:"rounds"`
	Plan     *PlanDoc        `json:"plan,omitempty"`
}

// CommentDraft is the body of POST /v1/reviews/{key}/comments.
type CommentDraft struct {
	Kind   ReviewKind `json:"kind,omitempty"`
	Anchor Anchor     `json:"anchor"`
	Text   string     `json:"text"`
}

// CommentEdit is the body of PATCH /v1/reviews/{key}/comments/{id}.
type CommentEdit struct {
	Text string `json:"text"`
}

// CommentReply is the body of POST /v1/reviews/{key}/comments/{id}/reply.
type CommentReply struct {
	Text string `json:"text"`
}

// ViewedMark is the body of PUT /v1/reviews/{key}/viewed.
type ViewedMark struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Viewed bool   `json:"viewed"`
}

// SubmitReview is the body of POST /v1/reviews/{key}/submit.
type SubmitReview struct {
	Kind          ReviewKind    `json:"kind,omitempty"`
	Verdict       ReviewVerdict `json:"verdict"`
	Summary       string        `json:"summary,omitempty"`
	PostToTracker bool          `json:"postToTracker"`
	Launch        *AgentLaunch  `json:"launch,omitempty"`
}

// ReviewReady is the body of POST /v1/reviews/{key}/ready.
type ReviewReady struct {
	Kind ReviewKind `json:"kind,omitempty"`
	Root string     `json:"root,omitempty"`
	Note string     `json:"note,omitempty"`
}

// MergeRequest is the body of POST /v1/reviews/{key}/merge.
type MergeRequest struct {
	RemoveGroup bool `json:"removeGroup"`
}

type MergeOutcome string

const (
	MergeMerged  MergeOutcome = "merged"
	MergeRebased MergeOutcome = "rebased"
	MergeStopped MergeOutcome = "conflict"
	MergeSkipped MergeOutcome = "skipped"
)

type RepoMerge struct {
	Name          string       `json:"name"`
	Result        MergeOutcome `json:"result"`
	Head          string       `json:"head,omitempty"`
	ConflictFiles []string     `json:"conflictFiles,omitempty"`
}

// MergeResult is the answer of POST /v1/reviews/{key}/merge. All or none.
type MergeResult struct {
	Merged    bool        `json:"merged"`
	Repos     []RepoMerge `json:"repos"`
	Removed   bool        `json:"removed"`
	Error     string      `json:"error,omitempty"`
	NotPushed []NotPushed `json:"notPushed"`
}

// The names of a ReviewEvent.
const (
	EventReviewReady    = "review.ready"
	EventReviewChanged  = "review.changed"
	EventReviewMerged   = "review.merged"
	EventReviewConflict = "review.conflict"
)

// ReviewEvent is the WebSocket frame of a review change. A hint: the app gets the data with GET.
type ReviewEvent struct {
	Type    string     `json:"type"` // review
	Event   string     `json:"event"`
	Key     string     `json:"key"`
	Kind    ReviewKind `json:"kind"`
	Machine string     `json:"machine,omitempty"`
}
