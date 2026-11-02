package main

import (
	"bytes"
	"strings"
	"testing"
)

// fakeEnv devolve um getenv que lê de um map, para o teste controlar o
// ambiente sem t.Setenv (que altera o processo inteiro e impede t.Parallel).
func fakeEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		// Garante que o binário sobe sem nenhuma variável definida, que é o
		// caso do container rodando com config mínima.
		{name: "sem variável usa o default", env: nil, want: defaultAddr},
		// Garante que a variável realmente tem efeito; sem este caso, um
		// loadConfig que ignorasse o ambiente passaria no teste acima.
		{name: "LLMUX_ADDR sobrescreve o default", env: map[string]string{"LLMUX_ADDR": ":9090"}, want: ":9090"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := loadConfig(fakeEnv(tt.env)).Addr
			if got != tt.want {
				t.Errorf("Addr = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRun(t *testing.T) {
	var out bytes.Buffer

	if err := run(fakeEnv(nil), &out); err != nil {
		t.Fatalf("run() erro inesperado: %v", err)
	}

	if !strings.Contains(out.String(), "addr="+defaultAddr) {
		t.Errorf("saída = %q, deveria conter %q", out.String(), "addr="+defaultAddr)
	}
}
