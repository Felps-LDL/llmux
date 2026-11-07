package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Felps-LDL/llmux/internal/config"
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
	"models": {"fast": [{"provider": "ollama", "model": "m"}]},
	"prices": [{"provider": "ollama", "model": "m", "input_per_million": 0, "output_per_million": 0}],
	"providers": {"ollama": {"base_url": "http://localhost:11434"}}
}`

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
	err := run(ctx, fakeEnv(map[string]string{"LLMUX_CONFIG": path, "LLMUX_ADDR": "127.0.0.1:0"}), &out)
	if err != nil {
		t.Fatalf("run() erro inesperado: %v", err)
	}

	if !strings.Contains(out.String(), "addr=127.0.0.1:") {
		t.Errorf("saída = %q, deveria conter %q", out.String(), "addr=127.0.0.1:")
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
