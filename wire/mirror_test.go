package wire

import "testing"

func TestMirrorFixtures(t *testing.T) {
	roundTrip[MirrorDoc](t, "mirror-doc")
	roundTrip[MirrorPut](t, "mirror-put")
	roundTrip[MirrorHead](t, "mirror-head")
	roundTrip[MirrorPage](t, "mirror-page")
	roundTrip[MirrorIndex](t, "mirror-index")
	roundTrip[MirrorFrame](t, "mirror-frame")
}

func TestMemberCallFixtures(t *testing.T) {
	roundTrip[MemberCall](t, "member-call")
	roundTrip[MemberCallRequest](t, "member-call-request")
	roundTrip[MemberCallFrame](t, "member-call-frame")
}
