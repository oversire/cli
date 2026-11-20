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
