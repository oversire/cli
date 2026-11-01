package wire

import "testing"

func TestReviewFixtures(t *testing.T) {
	roundTrip[ReviewsPage](t, "reviews-page")
	roundTrip[ReviewDetail](t, "review-detail")
	roundTrip[FileDiff](t, "file-diff")
	roundTrip[CommentDraft](t, "comment-draft")
	roundTrip[CommentEdit](t, "comment-edit")
	roundTrip[CommentReply](t, "comment-reply")
	roundTrip[ViewedMark](t, "viewed-mark")
	roundTrip[SubmitReview](t, "submit-review")
	roundTrip[ReviewRound](t, "review-round")
	roundTrip[ReviewReady](t, "review-ready")
	roundTrip[MergeRequest](t, "merge-request")
	roundTrip[MergeResult](t, "merge-result")
	roundTrip[PlanDoc](t, "plan-doc")
	roundTrip[ReviewEvent](t, "review-event")
}
