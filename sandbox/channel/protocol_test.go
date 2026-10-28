package channel

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Frame{Type: MsgHealthPing, ReqID: 7, Payload: []byte(`{"ok":true}`)}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFrame(&buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.ReqID != in.ReqID || string(out.Payload) != string(in.Payload) {
		t.Fatalf("got %+v", out)
	}
}

func TestFrameRejectsBadMagic(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader(make([]byte, headerSize)), 0)
	if err == nil {
		t.Fatal("expected bad magic")
	}
}

func TestFrameRejectsOversize(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 32)
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Type: MsgFilePutData, ReqID: 1, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrame(&buf, 8); err == nil {
		t.Fatal("expected oversize frame to be rejected")
	}
}
