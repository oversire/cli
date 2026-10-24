package wire

import (
	"bytes"
	"testing"
)

func frameRoundTrip(t *testing.T, kind string, decode func([]byte) (Message, error)) {
	t.Helper()
	for name, data := range fixtures(t, kind) {
		m, err := decode(data)
		if err != nil {
			t.Fatalf("%s/%s: %v", kind, name, err)
		}
		if _, ok := m.(Unknown); ok {
			t.Fatalf("%s/%s: unknown type", kind, name)
		}
		out, err := Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, kind+"/"+name, data, out)
	}
}

func TestStreamFixtures(t *testing.T) {
	frameRoundTrip(t, "stream-client-frame", DecodeClientFrame)
	frameRoundTrip(t, "stream-daemon-frame", DecodeDaemonFrame)
	roundTrip[SessionTurn](t, "session-turn")
}

func TestClientFrameIsStrict(t *testing.T) {
	for _, in := range []string{`{"type":"shell"}`, `{"type":"ack","seq":1,"x":2}`, `{"seq":1}`} {
		if _, err := DecodeClientFrame([]byte(in)); err == nil {
			t.Fatalf("%s: no error", in)
		}
	}
}

func TestBinaryFrame(t *testing.T) {
	b := EncodeBinary(OpOutput, 0x0102030405060708, []byte("hi"))
	if !bytes.Equal(b[:9], []byte{2, 1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("header %v", b[:9])
	}
	op, seq, data, err := DecodeBinary(b)
	if err != nil || op != OpOutput || seq != 0x0102030405060708 || string(data) != "hi" {
		t.Fatalf("got %v %x %q %v", op, seq, data, err)
	}
	if _, _, _, err := DecodeBinary([]byte{1, 0}); err == nil {
		t.Fatal("short frame decoded")
	}
	if _, _, _, err := DecodeBinary(make([]byte, 9)); err == nil {
		t.Fatal("op 0 decoded")
	}
}
