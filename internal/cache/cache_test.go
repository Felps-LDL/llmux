package cache_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/Felps-LDL/llmux/internal/cache"
	"github.com/Felps-LDL/llmux/internal/provider"
)

func ptr(f float64) *float64 { return &f }

func base() provider.ChatRequest {
	return provider.ChatRequest{
		Messages:    []provider.Message{{Role: provider.RoleUser, Content: "oi"}},
		Temperature: ptr(0),
		MaxTokens:   100,
	}
}

// Verifica a propriedade de que o cache depende: pedidos que podem ter respostas
// diferentes precisam ter chaves diferentes. Uma colisão aqui devolveria a resposta
// de outro pedido, sem erro nenhum. O caso da temperatura nula contra zero importa
// porque nil é "o padrão do provedor", que não é zero.
func TestKeyMudaComQualquerCampoQueMudaAResposta(t *testing.T) {
	mustKey := func(id int64, model string, req provider.ChatRequest) string {
		k, err := cache.Key(id, model, req)
		if err != nil {
			t.Fatalf("Key: %v", err)
		}
		return k
	}
	ref := mustKey(1, "fast", base())

	if again := mustKey(1, "fast", base()); again != ref {
		t.Fatalf("o mesmo pedido deu chaves diferentes: %q e %q", ref, again)
	}

	tests := []struct {
		name  string
		id    int64
		model string
		edit  func(*provider.ChatRequest)
	}{
		{name: "outra chave de API", id: 2, model: "fast", edit: func(*provider.ChatRequest) {}},
		{name: "outro modelo lógico", id: 1, model: "smart", edit: func(*provider.ChatRequest) {}},
		{name: "outro conteúdo", id: 1, model: "fast", edit: func(r *provider.ChatRequest) { r.Messages[0].Content = "olá" }},
		{name: "outro papel", id: 1, model: "fast", edit: func(r *provider.ChatRequest) { r.Messages[0].Role = provider.RoleSystem }},
		{name: "mensagem a mais", id: 1, model: "fast", edit: func(r *provider.ChatRequest) {
			r.Messages = append(r.Messages, provider.Message{Role: provider.RoleUser, Content: "oi"})
		}},
		{name: "temperatura nula", id: 1, model: "fast", edit: func(r *provider.ChatRequest) { r.Temperature = nil }},
		{name: "outro max_tokens", id: 1, model: "fast", edit: func(r *provider.ChatRequest) { r.MaxTokens = 101 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base()
			tt.edit(&req)
			if got := mustKey(tt.id, tt.model, req); got == ref {
				t.Errorf("chave igual à de referência: %q", got)
			}
		})
	}
}

var rdb *goredis.Client

func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := redis.Run(ctx, "redis:8-alpine")
	if err != nil {
		log.Fatalf("subindo o Redis: %v", err)
	}
	url, err := container.ConnectionString(ctx)
	if err != nil {
		log.Fatalf("lendo o endereço: %v", err)
	}
	opts, err := goredis.ParseURL(url)
	if err != nil {
		log.Fatalf("lendo o endereço: %v", err)
	}
	rdb = goredis.NewClient(opts)

	code := m.Run()

	_ = rdb.Close()
	if err := testcontainers.TerminateContainer(container); err != nil {
		log.Printf("derrubando o Redis: %v", err)
	}
	os.Exit(code)
}

// Verifica a ida e a volta pelo Redis e o prazo de validade. Sem o TTL, uma
// resposta ficaria no cache para sempre, mesmo depois de o provedor atualizar o
// modelo.
func TestSetEGetGuardamARespostaPeloTTL(t *testing.T) {
	c := cache.New(rdb, time.Hour)
	key, err := cache.Key(1, "fast", base())
	if err != nil {
		t.Fatalf("Key: %v", err)
	}

	if _, ok, err := c.Get(t.Context(), key); err != nil || ok {
		t.Fatalf("Get antes do Set = (ok %v, erro %v), want um miss sem erro", ok, err)
	}

	want := cache.Entry{
		Provider: "ollama",
		Model:    "llama3.2",
		Response: provider.ChatResponse{Model: "llama3.2", Content: "olá", FinishReason: "stop", Usage: provider.Usage{PromptTokens: 3, CompletionTokens: 2}},
	}
	if err := c.Set(t.Context(), key, want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok, err := c.Get(t.Context(), key)
	if err != nil || !ok {
		t.Fatalf("Get depois do Set = (ok %v, erro %v), want um acerto", ok, err)
	}
	if got != want {
		t.Errorf("Get = %+v, want %+v", got, want)
	}

	ttl, err := rdb.TTL(t.Context(), key).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > time.Hour {
		t.Errorf("TTL = %v, want até 1h", ttl)
	}
}

// Um valor corrompido no Redis precisa virar erro, e não uma resposta vazia
// devolvida como acerto.
func TestGetRecusaValorCorrompido(t *testing.T) {
	c := cache.New(rdb, time.Hour)
	key := "llmux:cache:corrompido"
	if err := rdb.Set(t.Context(), key, "isto não é JSON", time.Minute).Err(); err != nil {
		t.Fatalf("preparando: %v", err)
	}

	if _, ok, err := c.Get(t.Context(), key); err == nil || ok {
		t.Errorf("Get = (ok %v, erro %v), want erro e ok false", ok, err)
	}
}
