package config

import (
	"context"
	"strings"
	"testing"
)

func TestResolveLLMModel_DuplicateEnabledIDs(t *testing.T) {
	store := NewStore(nil)
	store.SetTemporary("provider.synth-b", `{"enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://b.example/v1","apiKey":"dummy-token-beta-bbbb","models":[{"id":"model-shared"}]}`)
	store.SetTemporary("provider.synth-a", `{"enabled":true,"clientType":"OpenAI Responses","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-shared"}]}`)
	store.SetTemporary("provider.synth-off", `{"enabled":false,"clientType":"OpenAI Compatible","baseUrl":"http://off.example/v1","apiKey":"dummy-token-off-oooo","models":[{"id":"model-shared"}]}`)
	b := NewBuilder(store, testLogger())
	_, err := b.ResolveLLMModel("model-shared")
	if err == nil || !strings.Contains(err.Error(), "synth-a") || !strings.Contains(err.Error(), "synth-b") {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, ok := b.GetLLMModel("model-shared"); ok {
		t.Fatal("duplicate model must not resolve to one provider")
	}
}

func TestResolveLLMModel_ResponsesStyle(t *testing.T) {
	store := NewStore(nil)
	store.SetTemporary("provider.synth-a", `{"enabled":true,"clientType":"OpenAI Responses","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main"}]}`)
	md, err := NewBuilder(store, testLogger()).ResolveLLMModel("model-sol-main")
	if err != nil {
		t.Fatal(err)
	}
	if md.APIStyle != "responses" || md.ChatPath != "" {
		t.Fatalf("style=%s path=%s", md.APIStyle, md.ChatPath)
	}
	if md.ProviderID != "synth-a" {
		t.Fatalf("provider id %s", md.ProviderID)
	}
}

func TestGetByPrefix_DBWinsOverEnv(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	const key = "provider.synth-shadow"
	const envVal = `{"enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://env.example/v1","apiKey":"from-env-dummy","models":[{"id":"model-shadow"}]}`
	const dbVal = `{"enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://db.example/v1","apiKey":"from-db-dummy","models":[{"id":"model-shadow"}]}`
	store.LoadEnvMap(map[string]string{key: envVal})
	if err := store.Set(context.Background(), key, dbVal); err != nil {
		t.Fatal(err)
	}
	got := store.GetByPrefix("provider.")
	if got["synth-shadow"] != dbVal {
		t.Fatal("prefix scan did not prefer the database value")
	}
	md, err := NewBuilder(store, testLogger()).ResolveLLMModel("model-shadow")
	if err != nil {
		t.Fatal(err)
	}
	if md.BaseURL != "http://db.example/v1" {
		t.Fatalf("base %s", md.BaseURL)
	}
}
