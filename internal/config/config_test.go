package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/config"
	"github.com/Felps-LDL/llmux/internal/usage"
)

const valid = `{
        "attempt_timeout": "30s",
        "breaker": {"threshold": 5, "cooldown": "1m"},
        "models": {"fast": [{"provider": "ollama", "model": "m"}]},
        "prices": [{"provider": "ollama", "model": "m", "input_per_million": 0, "output_per_million": 0}],
        "providers": {"ollama": {"base_url": "http://localhost:11434"}}
}`

func TestLoadAceitaConfigValida(t *testing.T) {
	cfg, err := config.Load(strings.NewReader(valid))
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if got := len(cfg.Models["fast"]); got != 1 {
		t.Errorf("rotas de fast = %d, want 1", got)
	}
	if got := len(cfg.Prices); got != 1 {
		t.Errorf("preços = %d, want 1", got)
	}

	if cfg.BreakerThreshold != 5 {
		t.Errorf("BreakerThreshold = %d, want 5", cfg.BreakerThreshold)
	}
	if cfg.AttemptTimeout != 30*time.Second {
		t.Errorf("AttemptTimeout = %v, want %v", cfg.AttemptTimeout, 30*time.Second)
	}
	if cfg.BreakerCooldown != time.Minute {
		t.Errorf("BreakerCooldown = %v, want %v", cfg.BreakerCooldown, time.Minute)
	}

	if got := cfg.Providers["ollama"].BaseURL; got != "http://localhost:11434" {
		t.Errorf("BaseURL = %q, want %q", got, "http://localhost:11434")
	}
}

func TestLoadRecusaConfigInvalida(t *testing.T) {
	tests := []struct {
		name string
		old  string // trecho de valid a substituir
		new  string
		want string // trecho esperado na mensagem de erro
	}{
		{name: "campo com erro de digitação", old: "attempt_timeout", new: "attempt_timout", want: "unknown field"},
		{name: "timeout zero", old: "30s", new: "0s", want: "attempt_timeout"},
		{name: "limite zero", old: `"threshold": 5`, new: `"threshold": 0`, want: "threshold"},
		{name: "preço sem saída", old: `, "output_per_million": 0`, new: ``, want: "entrada e saída"},
		{name: "cooldown zero", old: `"1m"`, new: `"0s"`, want: "cooldown"},
		{name: "preço negativo", old: `"input_per_million": 0`, new: `"input_per_million": -1`, want: "negativo"},
		{name: "preço duplicado", old: `"prices": [`, new: `"prices": [{"provider": "ollama", "model": "m", "input_per_million": 0, "output_per_million": 0}, `, want: "duplicado"},
		{name: "rota sem provider", old: `"provider": "ollama"`, new: `"provider": ""`, want: "sem provider"},
		{name: "sem modelos", old: `{"fast": [{"provider": "ollama", "model": "m"}]}`, new: `{}`, want: "pelo menos um modelo"},
		{name: "modelo sem rotas", old: `[{"provider": "ollama", "model": "m"}]`, new: `[]`, want: "não tem rotas"},
		{name: "provedor não configurado", old: `"providers": {"ollama": {"base_url": "http://localhost:11434"}}`, new: `"providers": {}`, want: "não está em providers"},
		{name: "provedor sem base_url", old: `"http://localhost:11434"`, new: `""`, want: "base_url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(strings.NewReader(strings.Replace(valid, tt.old, tt.new, 1)))
			if err == nil {
				t.Fatal("esperava erro")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("erro = %v, want %v", err.Error(), tt.want)
			}
		})
	}
}
func TestLoadRecusaRotaSemPreco(t *testing.T) {
	semPreco := strings.Replace(valid, `"model": "m", "input_per_million"`, `"model": "outro", "input_per_million"`, 1)

	_, err := config.Load(strings.NewReader(semPreco))

	if !errors.Is(err, usage.ErrUnknownPrice) {
		t.Fatalf("esperava erro ErrUnknownPrice, mas recebeu: %v", err)
	}
}

func TestLoadRecusaConteudoExtra(t *testing.T) {
	_, err := config.Load(strings.NewReader(valid + `{}`))
	if err == nil || !strings.Contains(err.Error(), "conteúdo extra") {
		t.Fatalf("esperava erro de conteúdo extra, mas recebeu: %v", err)
	}
}
