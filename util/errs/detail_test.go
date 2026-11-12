package errs

import (
	"strings"
	"testing"
)

func TestHTTPErrorWithDetail_ErrorOmitsBody(t *testing.T) {
	const reflected = "dummy-token-reflected-zzzz"
	err := HTTPErrorWithDetail(401, "http POST http://stub.example/v1/chat/completions -> 401", `{"error":{"message":"rejected `+reflected+`"}}`)
	if strings.Contains(err.Error(), reflected) {
		t.Fatalf("Error() leaked detail: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("status missing: %s", err.Error())
	}
	if !strings.Contains(ResponseDetail(err), reflected) {
		t.Fatal("detail was dropped")
	}
}
