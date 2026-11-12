package http

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestDumpAndErrorOmitReflectedSecret(t *testing.T) {
	const reflected = "dummy-token-reflected-zzzz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"rejected ` + reflected + `"}}`))
	}))
	defer srv.Close()

	core, recorded := observer.New(zapcore.DebugLevel)
	prev := log.Logger
	log.Logger = zap.New(core).Sugar()
	defer func() { log.Logger = prev }()

	client := New(WithBaseURL(srv.URL), WithTimeout(2*time.Second))
	_, err := client.Post("/v1/chat/completions").Dump().SetJSONBody(map[string]string{"model": "model-sol-main"}).Do()
	if err == nil {
		t.Fatal("expected status error")
	}
	if strings.Contains(err.Error(), reflected) {
		t.Fatalf("Error() leaked body: %s", err.Error())
	}
	if !strings.Contains(errs.ResponseDetail(err), reflected) {
		t.Fatal("detail missing")
	}
	for _, entry := range recorded.All() {
		if strings.Contains(entry.Message, reflected) {
			t.Fatalf("log message leaked: %s", entry.Message)
		}
		for _, field := range entry.Context {
			if strings.Contains(fieldToString(field), reflected) {
				t.Fatalf("log field %s leaked", field.Key)
			}
		}
	}
}

func fieldToString(field zapcore.Field) string {
	return field.Key + " " + field.String + " " + fmt.Sprint(field.Interface)
}
