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

func TestMirrorKindCI(t *testing.T) {
	for _, c := range []struct {
		k           MirrorKind
		valid, isCI bool
	}{
		{MirrorCIJobs, true, true}, {MirrorCIChecks, true, true}, {MirrorCIState, true, true},
		{MirrorRuns, true, false}, {"made", false, false},
	} {
		if c.k.Valid() != c.valid || c.k.IsCI() != c.isCI {
			t.Errorf("%s: valid %v, ci %v", c.k, c.k.Valid(), c.k.IsCI())
		}
	}
}

func TestMirrorDocCIJobs(t *testing.T) {
	for name, data := range fixtures(t, "mirror-doc") {
		var d MirrorDoc
		if err := Decode(data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Kind != MirrorCIJobs {
			continue
		}
		var b CIJobsDoc
		if err := Decode(d.Body, &b); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b.Run != "9001" || b.Attempt != 1 || b.Final {
			t.Errorf("%s: %+v", name, b)
		}
		return
	}
	t.Fatal("no ciJobs fixture")
}
