package memory

import "testing"

func TestHashEmbedStableAndNormalized(t *testing.T) {
	a := hashEmbed("只转发经典黑白女仆装")
	b := hashEmbed("只转发经典黑白女仆装")
	if len(a) != vecDims*4 || string(a) != string(b) {
		t.Fatalf("embed not stable: %d", len(a))
	}
	if string(hashEmbed("女仆服偏好")) == string(a) {
		t.Fatal("different text should not hash equal")
	}
}

func TestOpenVecIndexNilWithoutDB(t *testing.T) {
	if OpenVecIndex(nil) != nil {
		t.Fatal("nil db must not enable vec")
	}
}
