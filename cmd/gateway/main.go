package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Felps-LDL/llmux/internal/api"
	"github.com/Felps-LDL/llmux/internal/config"
	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/anthropic"
	"github.com/Felps-LDL/llmux/internal/provider/ollama"
	"github.com/Felps-LDL/llmux/internal/provider/openai"
	"github.com/Felps-LDL/llmux/internal/router"
)

const (
	defaultAddr       = ":8080"
	defaultConfigPath = "config.json"

	// Sem limite para ler os cabeçalhos, clientes lentos prenderiam conexões abertas.
	// Não há WriteTimeout porque ele cortaria uma resposta longa de um LLM; o prazo
	// de cada chamada já é controlado pelo router.
	readHeaderTimeout = 5 * time.Second
	// Tempo para os pedidos em andamento terminarem depois de um sinal de encerramento.
	shutdownTimeout = 30 * time.Second

	// Os mesmos nomes que os SDKs oficiais usam: quem já tem a chave no ambiente não
	// precisa mudar nada. A chave nunca vai na config, que tem um exemplo versionado.
	openAIKeyEnv    = "OPENAI_API_KEY"
	anthropicKeyEnv = "ANTHROPIC_API_KEY"
)

type settings struct {
	Addr       string
	ConfigPath string
}

// getenv é injetado para o teste fornecer um ambiente falso sem mexer no
// ambiente real do processo.
func loadSettings(getenv func(string) string) settings {
	s := settings{Addr: defaultAddr, ConfigPath: defaultConfigPath}
	if v := getenv("LLMUX_ADDR"); v != "" {
		s.Addr = v
	}
	if v := getenv("LLMUX_CONFIG"); v != "" {
		s.ConfigPath = v
	}
	return s
}

// buildRouter cria um cliente por provedor e monta as rotas da config. Falha para
// nome desconhecido ou chave ausente, porque um erro de configuração precisa
// aparecer na subida, e não como um 502 em cada pedido.
func buildRouter(cfg *config.Config, getenv func(string) string) (*router.Router, error) {
	providers := make(map[string]provider.Provider, len(cfg.Providers))
	for name, p := range cfg.Providers {
		switch name {
		case "ollama":
			providers[name] = ollama.New(p.BaseURL, &http.Client{})
		case "openai":
			key, err := apiKey(getenv, name, openAIKeyEnv)
			if err != nil {
				return nil, err
			}
			providers[name] = openai.New(p.BaseURL, key, &http.Client{})
		case "anthropic":
			key, err := apiKey(getenv, name, anthropicKeyEnv)
			if err != nil {
				return nil, err
			}
			providers[name] = anthropic.New(p.BaseURL, key, &http.Client{})
		default:
			return nil, fmt.Errorf("provedor %q não é suportado", name)
		}
	}

	routes := make(map[string][]router.Route, len(cfg.Models))
	for model, modelRoutes := range cfg.Models {
		for _, r := range modelRoutes {
			routes[model] = append(routes[model], router.Route{Provider: providers[r.Provider], Model: r.Model})
		}
	}

	return router.New(routes, cfg.AttemptTimeout, router.BreakerConfig{
		Threshold: cfg.BreakerThreshold,
		Cooldown:  cfg.BreakerCooldown,
	}), nil
}

// apiKey lê a chave de um provedor pago. Sem ela, o gateway não sobe: senão cada
// pedido viraria um 502 por causa de um 401 visível só no log.
func apiKey(getenv func(string) string, providerName, env string) (string, error) {
	key := getenv(env)
	if key == "" {
		return "", fmt.Errorf("provedor %q precisa da variável %s", providerName, env)
	}
	return key, nil
}

func loadConfig(path string) (*config.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("abrindo config: %w", err)
	}
	defer func() { _ = f.Close() }()

	return config.Load(f)
}

// serve atende até o ctx ser cancelado e então espera os pedidos em andamento.
func serve(ctx context.Context, ln net.Listener, handler http.Handler) error {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("servindo: %w", err)
	case <-ctx.Done():
	}

	// Um contexto novo porque o ctx já foi cancelado, e o Shutdown desistiria na hora
	// em vez de esperar os pedidos em andamento.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("encerrando: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("servindo: %w", err)
	}
	return nil
}

// run concentra a lógica fora do main para ser testável: retorna erro em vez
// de chamar os.Exit e escreve num io.Writer em vez de os.Stdout.
func run(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	s := loadSettings(getenv)

	cfg, err := loadConfig(s.ConfigPath)
	if err != nil {
		return err
	}
	r, err := buildRouter(cfg, getenv)
	if err != nil {
		return err
	}

	// A porta é aberta antes de anunciar o endereço, para uma porta ocupada ser um
	// erro na subida. Usa context.Background porque o ctx só significa "encerrar".
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("abrindo porta: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "llmux gateway: addr=%s\n", ln.Addr()); err != nil {
		_ = ln.Close()
		return fmt.Errorf("escrevendo saída: %w", err)
	}

	return serve(ctx, ln, api.New(r, cfg.Prices).Handler())
}

func main() {
	// SIGTERM é o sinal que o Kubernetes envia antes de encerrar um pod.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n", err)
		os.Exit(1)
	}
}
