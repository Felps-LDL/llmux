// Package config carrega e valida a configuração do gateway.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Felps-LDL/llmux/internal/usage"
)

// Route liga um modelo lógico a um provedor e ao nome físico do modelo nele.
type Route struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Provider guarda como alcançar um provedor.
type Provider struct {
	BaseURL string `json:"base_url"`
}

// RateLimit são os limites por minuto das chaves que não têm limites próprios.
type RateLimit struct {
	RequestsPerMinute int `json:"requests_per_minute"`
	TokensPerMinute   int `json:"tokens_per_minute"`
}

// Config é a configuração já validada.
type Config struct {
	AttemptTimeout   time.Duration
	BreakerThreshold int
	BreakerCooldown  time.Duration
	RateLimit        RateLimit
	Providers        map[string]Provider
	Models           map[string][]Route
	Prices           usage.PriceTable
}

// duration existe porque time.Duration em JSON é um número de nanossegundos, e
// "30s" é mais difícil de errar do que 30000000000.
type duration time.Duration

func (d *duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duração precisa ser texto, como \"30s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duração inválida: %w", err)
	}
	*d = duration(parsed)
	return nil
}

type rawPrice struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Ponteiros porque um campo esquecido viraria preço zero em silêncio: nil é
	// "não informado", e 0 escrito é "de graça" (ver ADR 0002).
	InputPerMillion  *int64 `json:"input_per_million"`
	OutputPerMillion *int64 `json:"output_per_million"`
}

type rawConfig struct {
	AttemptTimeout duration `json:"attempt_timeout"`
	Breaker        struct {
		Threshold int      `json:"threshold"`
		Cooldown  duration `json:"cooldown"`
	} `json:"breaker"`
	RateLimit RateLimit           `json:"rate_limit"`
	Providers map[string]Provider `json:"providers"`
	Models    map[string][]Route  `json:"models"`
	Prices    []rawPrice          `json:"prices"`
}

// Load lê e valida a configuração. Devolve todos os problemas de uma vez, para
// quem corrige o arquivo não descobrir um erro por tentativa.
func Load(r io.Reader) (*Config, error) {
	dec := json.NewDecoder(r)
	// Sem isso, um campo com erro de digitação seria ignorado, e o valor certo
	// ficaria zerado sem aviso.
	dec.DisallowUnknownFields()

	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: lendo JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("config: conteúdo extra depois do JSON")
	}

	var problems []error
	if raw.AttemptTimeout <= 0 {
		problems = append(problems, errors.New("attempt_timeout precisa ser positivo"))
	}
	if raw.Breaker.Threshold <= 0 {
		problems = append(problems, errors.New("breaker.threshold precisa ser positivo"))
	}
	// Zero faria um provedor morto receber uma sonda a cada pedido.
	if raw.Breaker.Cooldown <= 0 {
		problems = append(problems, errors.New("breaker.cooldown precisa ser positivo"))
	}

	// Sem padrão, uma chave sem limites próprios não teria balde: o bloco é
	// obrigatório, como os preços.
	if raw.RateLimit.RequestsPerMinute <= 0 || raw.RateLimit.TokensPerMinute <= 0 {
		problems = append(problems, errors.New("rate_limit precisa de requests_per_minute e tokens_per_minute positivos"))
	}

	for name, p := range raw.Providers {
		if p.BaseURL == "" {
			problems = append(problems, fmt.Errorf("provedor %q precisa de base_url", name))
		}
	}

	prices := make(usage.PriceTable, len(raw.Prices))
	for _, p := range raw.Prices {
		key := usage.ModelKey{Provider: p.Provider, Model: p.Model}
		if p.InputPerMillion == nil || p.OutputPerMillion == nil {
			problems = append(problems, fmt.Errorf("preço de %s/%s precisa de entrada e saída", p.Provider, p.Model))
			continue
		}
		if *p.InputPerMillion < 0 || *p.OutputPerMillion < 0 {
			problems = append(problems, fmt.Errorf("preço de %s/%s não pode ser negativo", p.Provider, p.Model))
			continue
		}
		if _, dup := prices[key]; dup {
			problems = append(problems, fmt.Errorf("preço de %s/%s duplicado", p.Provider, p.Model))
			continue
		}
		prices[key] = usage.Price{InputPerMillion: *p.InputPerMillion, OutputPerMillion: *p.OutputPerMillion}
	}

	if len(raw.Models) == 0 {
		problems = append(problems, errors.New("models precisa ter pelo menos um modelo"))
	}
	for name, routes := range raw.Models {
		if len(routes) == 0 {
			problems = append(problems, fmt.Errorf("modelo %q não tem rotas", name))
		}
		for _, route := range routes {
			if route.Provider == "" || route.Model == "" {
				problems = append(problems, fmt.Errorf("modelo %q tem rota sem provider ou model", name))
				continue
			}
			if _, ok := raw.Providers[route.Provider]; !ok {
				problems = append(problems, fmt.Errorf("modelo %q usa o provedor %q, que não está em providers", name, route.Provider))
			}
			// Aqui o gateway se recusa a subir sem preço, em vez de zerar o custo em
			// produção (ADR 0002). Reutiliza o erro de usage: é o mesmo problema.
			if _, ok := prices[usage.ModelKey{Provider: route.Provider, Model: route.Model}]; !ok {
				problems = append(problems, fmt.Errorf("%w: rota %q usa %s/%s", usage.ErrUnknownPrice, name, route.Provider, route.Model))
			}
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("config inválida: %w", errors.Join(problems...))
	}

	return &Config{
		AttemptTimeout:   time.Duration(raw.AttemptTimeout),
		BreakerThreshold: raw.Breaker.Threshold,
		BreakerCooldown:  time.Duration(raw.Breaker.Cooldown),
		RateLimit:        raw.RateLimit,
		Providers:        raw.Providers,
		Models:           raw.Models,
		Prices:           prices,
	}, nil
}
