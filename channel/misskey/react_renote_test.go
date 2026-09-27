package misskey

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// fakeReactServer serves notes/show from notes and records reaction targets.
func fakeReactServer(t *testing.T, notes map[string]string) (*httptest.Server, *[]string) {
	t.Helper()
	var reacted []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		id, _ := req["noteId"].(string)
		switch {
		case strings.HasSuffix(r.URL.Path, "/notes/show"):
			if n, ok := notes[id]; ok {
				_, _ = w.Write([]byte(n))
				return
			}
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"NO_SUCH_NOTE","message":"No such note."}}`))
		case strings.HasSuffix(r.URL.Path, "/notes/reactions/create"):
			reacted = append(reacted, id)
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &reacted
}

func reactTool(c *MisskeyChannel) llm.Tool {
	return c.reactToNoteTool().Tool
}

func TestReactToNote_PureRenoteResolvesToOriginal(t *testing.T) {
	ts, reacted := fakeReactServer(t, map[string]string{
		"rn1":  `{"id":"rn1","text":"","renoteId":"orig","user":{"username":"a"}}`,
		"orig": `{"id":"orig","text":"hello","user":{"username":"b"}}`,
	})
	c := &MisskeyChannel{name: "Misskey", api: newAPIClient(ts.URL, "tok")}
	res, err := reactTool(c).Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{"noteId": "rn1", "reaction": "👍"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["success"] != true || m["noteId"] != "orig" || m["renoteOf"] != "orig" || m["requestedNoteId"] != "rn1" {
		t.Fatalf("unexpected result %#v", m)
	}
	if len(*reacted) != 1 || (*reacted)[0] != "orig" {
		t.Fatalf("reaction must go to the original note, got %v", *reacted)
	}
}

func TestReactToNote_QuoteRenoteIsReactable(t *testing.T) {
	ts, reacted := fakeReactServer(t, map[string]string{
		"q1": `{"id":"q1","text":"my comment","renoteId":"orig","user":{"username":"a"}}`,
	})
	c := &MisskeyChannel{name: "Misskey", api: newAPIClient(ts.URL, "tok")}
	res, err := reactTool(c).Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{"noteId": "q1", "reaction": "👍"})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["noteId"] != "q1" || m["renoteOf"] != nil {
		t.Fatalf("a quote renote must be reacted to directly: %#v", m)
	}
	if len(*reacted) != 1 || (*reacted)[0] != "q1" {
		t.Fatalf("got %v", *reacted)
	}
}

func TestNoteIsPureRenote(t *testing.T) {
	cases := []struct {
		n    Note
		want bool
	}{
		{Note{RenoteID: "x"}, true},
		{Note{RenoteID: "x", Text: "quote"}, false},
		{Note{RenoteID: "x", CW: "cw"}, false},
		{Note{RenoteID: "x", Files: []File{{ID: "f"}}}, false},
		{Note{RenoteID: "x", Poll: map[string]any{}}, false},
		{Note{Text: "plain"}, false},
	}
	for i, c := range cases {
		if got := c.n.IsPureRenote(); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}
