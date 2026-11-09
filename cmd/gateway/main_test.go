package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Felps-LDL/llmux/internal/config"
	"github.com/Felps-LDL/llmux/internal/store"
)

// fakeEnv devolve um getenv que lê de um map, para o teste controlar o
// ambiente sem t.Setenv (que altera o processo inteiro e impede t.Parallel).
func fakeEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadSettings(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		// Garante que o binário sobe sem nenhuma variável definida, que é o
		// caso do container rodando com config mínima.
		{name: "sem variável usa o default", env: nil, want: defaultAddr},
		// Garante que a variável realmente tem efeito; sem este caso, um
		// loadSettings que ignorasse o ambiente passaria no teste acima.
		{name: "LLMUX_ADDR sobrescreve o default", env: map[string]string{"LLMUX_ADDR": ":9090"}, want: ":9090"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := loadSettings(fakeEnv(tt.env)).Addr
			if got != tt.want {
				t.Errorf("Addr = %q, want %q", got, tt.want)
			}
		})
	}
}

const testConfig = `{
	"attempt_timeout": "30s",
	"breaker": {"threshold": 5, "cooldown": "1m"},
	"rate_limit": {"requests_per_minute": 60, "tokens_per_minute": 100000},
	"cache": {"ttl": "1h"},
	"models": {"fast": [{"provider": "ollama", "model": "m"}]},
	"prices": [{"provider": "ollama", "model": "m", "input_per_million": 0, "output_per_million": 0}],
	"providers": {"ollama": {"base_url": "http://localhost:11434"}}
}`

// O Postgres sobe sob demanda e uma vez só: os testes que não usam banco não pagam
// os segundos da subida, e os que usam dividem o mesmo container.
var (
	pgOnce      sync.Once
	pgContainer *postgres.PostgresContainer
	pgURL       string
	pgErr       error
)

func databaseURL(t *testing.T) string {
	t.Helper()
	pgOnce.Do(func() {
		ctx := context.Background()
		pgContainer, pgErr = postgres.Run(ctx, "postgres:18-alpine",
			postgres.WithDatabase("llmux"), postgres.WithUsername("llmux"), postgres.WithPassword("llmux"),
			postgres.BasicWaitStrategies())
		if pgErr == nil {
			pgURL, pgErr = pgContainer.ConnectionString(ctx, "sslmode=disable")
		}
	})
	if pgErr != nil {
		t.Fatalf("subindo o Postgres: %v", pgErr)
	}
	return pgURL
}

func TestMain(m *testing.M) {
	code := m.Run()
	if pgContainer != nil {
		_ = testcontainers.TerminateContainer(pgContainer)
	}
	os.Exit(code)
}

func TestRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(testConfig), 0o600); err != nil {
		t.Fatalf("escrevendo config: %v", err)
	}

	// Cancelado de antemão: run sobe e encerra na hora, como se tivesse recebido um
	// sinal. A porta 0 pede uma porta livre, para o teste não depender de nenhuma.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var out bytes.Buffer
	err := run(ctx, nil, fakeEnv(map[string]string{
		"LLMUX_CONFIG":       path,
		"LLMUX_ADDR":         "127.0.0.1:0",
		"LLMUX_DATABASE_URL": databaseURL(t),
		// Uma porta sem Redis: o gateway precisa subir mesmo assim, com um aviso,
		// porque o rate limit falha aberto.
		"LLMUX_REDIS_URL": "redis://127.0.0.1:1/0",
	}), &out)
	if err != nil {
		t.Fatalf("run() erro inesperado: %v", err)
	}
	if !strings.Contains(out.String(), "rate limit desligado") {
		t.Errorf("saída = %q, want o aviso do Redis indisponível", out.String())
	}

	if !strings.Contains(out.String(), "addr=127.0.0.1:") {
		t.Errorf("saída = %q, deveria conter %q", out.String(), "addr=127.0.0.1:")
	}
}

// Verifica que o gateway não sobe sem as variáveis dos serviços. Sem o banco não
// há autenticação, e um gateway aberto por esquecimento de uma variável é o pior
// resultado possível. Sem a do Redis, o rate limit ficaria desligado em silêncio.
func TestRunNaoSobeSemVariaveisDosServicos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(testConfig), 0o600); err != nil {
		t.Fatalf("escrevendo config: %v", err)
	}

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "sem banco", env: map[string]string{}, want: "LLMUX_DATABASE_URL"},
		{name: "sem Redis", env: map[string]string{"LLMUX_DATABASE_URL": databaseURL(t)}, want: "LLMUX_REDIS_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.env["LLMUX_CONFIG"] = path
			tt.env["LLMUX_ADDR"] = "127.0.0.1:0"
			err := run(t.Context(), nil, fakeEnv(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("erro = %v, want um erro citando %s", err, tt.want)
			}
		})
	}
}

// Verifica o ciclo de vida de uma chave pela linha de comando, contra o banco de
// verdade: a chave impressa por create é a que o store encontra, e revoke a tira
// de circulação. É o caminho que um operador usa, sem interface de administração.
func TestKeysCriaERevoga(t *testing.T) {
	env := fakeEnv(map[string]string{"LLMUX_DATABASE_URL": databaseURL(t)})

	var out bytes.Buffer
	if err := run(t.Context(), []string{"keys", "create", "-name", "cliente-a"}, env, &out); err != nil {
		t.Fatalf("keys create: %v", err)
	}
	var id int64
	var secret string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "key="); ok {
			secret = v
		}
		if v, ok := strings.CutPrefix(line, "id="); ok {
			_, _ = fmt.Sscanf(v, "%d", &id)
		}
	}
	if secret == "" || id == 0 {
		t.Fatalf("saída de create sem id ou chave: %q", out.String())
	}

	pool, err := pgxpool.New(t.Context(), databaseURL(t))
	if err != nil {
		t.Fatalf("conectando: %v", err)
	}
	defer pool.Close()
	st := store.New(pool)
	if _, err := st.LookupKey(t.Context(), secret); err != nil {
		t.Fatalf("a chave impressa não foi encontrada: %v", err)
	}

	if err := run(t.Context(), []string{"keys", "revoke", "-id", fmt.Sprint(id)}, env, io.Discard); err != nil {
		t.Fatalf("keys revoke: %v", err)
	}
	if _, err := st.LookupKey(t.Context(), secret); !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("após revoke: erro = %v, want %v", err, store.ErrKeyNotFound)
	}
}

// Erros de uso não podem depender do banco: o ambiente aqui não tem
// LLMUX_DATABASE_URL, e o erro precisa ser o de uso, não o da variável.
func TestKeysRecusaUsoErradoSemTocarNoBanco(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "sem subcomando", args: []string{"keys"}, want: "uso:"},
		{name: "subcomando desconhecido", args: []string{"keys", "delete"}, want: "uso:"},
		{name: "create sem nome", args: []string{"keys", "create"}, want: "-name"},
		{name: "revoke sem id", args: []string{"keys", "revoke"}, want: "-id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(t.Context(), tt.args, fakeEnv(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("erro = %v, want um erro citando %q", err, tt.want)
			}
		})
	}
}

// Verifica a regra da chave: com um provedor pago na config, o gateway só sobe se
// a chave dele estiver no ambiente. O caso só com o Ollama importa tanto quanto:
// ninguém pode ser obrigado a definir uma chave que nunca vai usar. O caso da
// chave trocada prova que cada provedor lê a própria variável.
func TestBuildRouterExigeChaveSoDeQuemUsa(t *testing.T) {
	tests := []struct {
		name      string
		providers string
		env       map[string]string
		wantErr   bool
		// O erro precisa dizer o que falta; senão o operador procura no código.
		errMentions string
	}{
		{name: "openai sem chave", providers: `"openai": {"base_url": "http://x/v1"}`, wantErr: true, errMentions: "OPENAI_API_KEY"},
		{name: "openai com chave", providers: `"openai": {"base_url": "http://x/v1"}`, env: map[string]string{"OPENAI_API_KEY": "k"}},
		{name: "anthropic sem chave", providers: `"anthropic": {"base_url": "http://x"}`, wantErr: true, errMentions: "ANTHROPIC_API_KEY"},
		{name: "anthropic com chave", providers: `"anthropic": {"base_url": "http://x"}`, env: map[string]string{"ANTHROPIC_API_KEY": "k"}},
		{name: "anthropic com a chave da openai", providers: `"anthropic": {"base_url": "http://x"}`, env: map[string]string{"OPENAI_API_KEY": "k"}, wantErr: true, errMentions: "ANTHROPIC_API_KEY"},
		{name: "só ollama, sem chave", providers: `"ollama": {"base_url": "http://x"}`},
		{name: "provedor desconhecido", providers: `"azure": {"base_url": "http://x"}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := strings.Split(tt.providers, `"`)[1]
			cfg, err := config.Load(strings.NewReader(`{
				"attempt_timeout": "30s",
				"breaker": {"threshold": 5, "cooldown": "1m"},
	"rate_limit": {"requests_per_minute": 60, "tokens_per_minute": 100000},
	"cache": {"ttl": "1h"},
				"providers": {` + tt.providers + `},
				"models": {"fast": [{"provider": "` + name + `", "model": "m"}]},
				"prices": [{"provider": "` + name + `", "model": "m", "input_per_million": 0, "output_per_million": 0}]
			}`))
			if err != nil {
				t.Fatalf("config: %v", err)
			}

			_, err = buildRouter(cfg, fakeEnv(tt.env))
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("erro = %v, want erro: %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tt.errMentions) {
				t.Errorf("erro = %q, deveria citar %q", err, tt.errMentions)
			}
		})
	}
}
