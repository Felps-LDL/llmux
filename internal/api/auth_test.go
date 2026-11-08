package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/api"
	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/router"
	"github.com/Felps-LDL/llmux/internal/store"
)

// lookupFunc é um KeyLookup definido pelo teste, para os casos que o map não
// expressa, como o banco fora do ar.
type lookupFunc func(ctx context.Context, secret string) (store.APIKey, error)

func (f lookupFunc) LookupKey(ctx context.Context, secret string) (store.APIKey, error) {
	return f(ctx, secret)
}

func newAuthHandler(p provider.Provider, k api.KeyLookup) http.Handler {
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: p, Model: "modelo-fisico"}},
	}, time.Second, router.BreakerConfig{Threshold: 100, Cooldown: time.Minute})
	return api.New(r, nil, k).Handler()
}

// Verifica cada forma de um pedido não passar, e que nenhuma chega ao provedor:
// um pedido sem chave válida que chegasse lá custaria dinheiro a quem não pagou.
// O caso do banco fora do ar é o único que não é 401, pelo motivo em authenticate.
// Os cabeçalhos malformados nem podem chegar ao banco: senão qualquer pedido lixo
// viraria uma consulta.
func TestAutenticacaoBarraPedidoSemChaveValida(t *testing.T) {
	errBanco := errors.New("conexão recusada")
	valid := keys{"llmux_boa": {ID: 1}}

	tests := []struct {
		name       string
		header     string
		keys       api.KeyLookup
		wantStatus int
		wantLookup bool
	}{
		{name: "sem cabeçalho", header: "", keys: valid, wantStatus: http.StatusUnauthorized},
		{name: "esquema errado", header: "Basic llmux_boa", keys: valid, wantStatus: http.StatusUnauthorized},
		{name: "bearer vazio", header: "Bearer ", keys: valid, wantStatus: http.StatusUnauthorized},
		{name: "chave desconhecida", header: "Bearer llmux_ruim", keys: valid, wantStatus: http.StatusUnauthorized, wantLookup: true},
		{
			name:   "banco fora do ar",
			header: "Bearer llmux_boa",
			keys: lookupFunc(func(context.Context, string) (store.APIKey, error) {
				return store.APIKey{}, errBanco
			}),
			wantStatus: http.StatusServiceUnavailable,
			wantLookup: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
			body := `{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()

			looked := false
			k := lookupFunc(func(ctx context.Context, secret string) (store.APIKey, error) {
				looked = true
				return tt.keys.LookupKey(ctx, secret)
			})
			newAuthHandler(a, k).ServeHTTP(rec, req)

			if looked != tt.wantLookup {
				t.Errorf("consultou o store = %v, want %v", looked, tt.wantLookup)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (corpo: %s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := len(a.Calls()); got != 0 {
				t.Errorf("chamadas ao provedor = %d, want 0", got)
			}
			if tt.wantStatus == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", rec.Header().Get("WWW-Authenticate"))
			}
			// O erro do banco pode trazer detalhes internos; eles ficam no log.
			if strings.Contains(rec.Body.String(), errBanco.Error()) {
				t.Errorf("corpo expõe o erro interno: %s", rec.Body)
			}
		})
	}
}

// Verifica o caminho feliz e o que chega ao store. O esquema em minúsculas prova
// que a comparação ignora maiúsculas, como a RFC manda; e o segredo precisa chegar
// sem o "Bearer ", senão nenhuma chave seria encontrada.
func TestAutenticacaoDeixaPassarChaveValida(t *testing.T) {
	var got string
	k := lookupFunc(func(_ context.Context, secret string) (store.APIKey, error) {
		got = secret
		return store.APIKey{ID: 1}, nil
	})
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	body := `{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "bearer llmux_boa")
	rec := httptest.NewRecorder()

	newAuthHandler(a, k).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (corpo: %s)", rec.Code, rec.Body)
	}
	if got != "llmux_boa" {
		t.Errorf("segredo consultado = %q, want %q", got, "llmux_boa")
	}
	if n := len(a.Calls()); n != 1 {
		t.Errorf("chamadas ao provedor = %d, want 1", n)
	}
}

func TestAutenticacaoNaoRespondeClienteQueDesistiu(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	k := lookupFunc(func(ctx context.Context, _ string) (store.APIKey, error) {
		// O cliente desiste durante a consulta, e o pgx devolve o erro do contexto.
		cancel()
		return store.APIKey{}, ctx.Err()
	})
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	body := `{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer llmux_boa")
	rec := httptest.NewRecorder()

	newAuthHandler(a, k).ServeHTTP(rec, req)

	if rec.Body.Len() != 0 {
		t.Errorf("corpo = %q, want vazio", rec.Body)
	}
}
