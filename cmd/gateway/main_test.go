package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
