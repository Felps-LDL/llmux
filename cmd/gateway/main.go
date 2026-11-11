package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Felps-LDL/llmux/internal/api"
	"github.com/Felps-LDL/llmux/internal/cache"
	"github.com/Felps-LDL/llmux/internal/config"
	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/anthropic"
	"github.com/Felps-LDL/llmux/internal/provider/ollama"
	"github.com/Felps-LDL/llmux/internal/provider/openai"
	"github.com/Felps-LDL/llmux/internal/ratelimit"
	"github.com/Felps-LDL/llmux/internal/router"
	"github.com/Felps-LDL/llmux/internal/store"
	"github.com/Felps-LDL/llmux/internal/telemetry"
)

const (
	defaultAddr = ":8080"
	// Porta própria para as métricas, fora da porta pública: elas mostram custos e
	// ids de chave. 2112 é a do guia do client_golang; a 9090 é a do próprio
	// Prometheus, que vai rodar ao lado no compose.
	defaultMetricsAddr = ":2112"
	defaultConfigPath  = "config.json"

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

	databaseURLEnv = "LLMUX_DATABASE_URL"
	redisURLEnv    = "LLMUX_REDIS_URL"
	// Prazo para alcançar o banco na subida: no compose, o healthcheck já garantiu
	// que ele está pronto, então esperar mais só atrasaria um erro de configuração.
	dbConnectTimeout = 10 * time.Second
	// O mesmo limite da trava das migrations (store.Migrate): uma réplica pode
	// esperar outra terminar de migrar.
	migrateTimeout = 5 * time.Minute
)

type settings struct {
	Addr        string
	MetricsAddr string
	ConfigPath  string
	DatabaseURL string
	RedisURL    string
	LogLevel    string
}

// getenv é injetado para o teste fornecer um ambiente falso sem mexer no
// ambiente real do processo.
func loadSettings(getenv func(string) string) settings {
	s := settings{Addr: defaultAddr, MetricsAddr: defaultMetricsAddr, ConfigPath: defaultConfigPath}
	if v := getenv("LLMUX_METRICS_ADDR"); v != "" {
		s.MetricsAddr = v
	}
	if v := getenv("LLMUX_ADDR"); v != "" {
		s.Addr = v
	}
	if v := getenv("LLMUX_CONFIG"); v != "" {
		s.ConfigPath = v
	}
	s.DatabaseURL = getenv(databaseURLEnv)
	s.RedisURL = getenv(redisURLEnv)
	s.LogLevel = getenv("LLMUX_LOG_LEVEL")
	return s
}

// setupLogger troca o logger padrão do processo pelo JSON do gateway. É o padrão, e
// não um logger passado de função em função, porque o request_id viaja no contexto:
// qualquer camada chama slog.InfoContext e a linha sai com o id.
func setupLogger(level string, w io.Writer) error {
	var l slog.Level // o zero é Info
	if level != "" {
		if err := l.UnmarshalText([]byte(level)); err != nil {
			return fmt.Errorf("LLMUX_LOG_LEVEL inválido (use debug, info, warn ou error): %w", err)
		}
	}
	slog.SetDefault(telemetry.NewLogger(w, l))
	return nil
}

// openRedis cria o cliente do rate limit. A variável ausente é erro de
// configuração e impede a subida; o Redis fora do ar, não: o rate limit falha
// aberto (ver api.Server.reserve), e subir sem ele é melhor que não subir.
func openRedis(redisURL string) (*redis.Client, error) {
	if redisURL == "" {
		return nil, fmt.Errorf("a variável %s é obrigatória", redisURLEnv)
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("lendo %s: %w", redisURLEnv, err)
	}
	rdb := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Warn("Redis indisponível, rate limit e cache desligados até ele voltar", "error", err)
	}
	return rdb, nil
}

// openStore conecta ao Postgres e aplica as migrations. Usa prazos próprios, e não
// o ctx de run, que só significa "encerrar".
func openStore(databaseURL string) (*pgxpool.Pool, error) {
	// Sem banco não há como verificar chaves, e um gateway sem autenticação não
	// deve subir por engano.
	if databaseURL == "" {
		return nil, fmt.Errorf("a variável %s é obrigatória", databaseURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("configurando o banco: %w", err)
	}
	// pgxpool.New não conecta; sem o Ping, um banco fora do ar só apareceria no
	// primeiro pedido.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("conectando ao banco: %w", err)
	}

	migrateCtx, cancelMigrate := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancelMigrate()
	if err := store.Migrate(migrateCtx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
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
// de chamar os.Exit e escreve num io.Writer em vez de os.Stdout. args são os
// argumentos sem o nome do programa.
func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	s := loadSettings(getenv)
	if err := setupLogger(s.LogLevel, stdout); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "keys" {
		return runKeys(args[1:], s, stdout)
	}

	cfg, err := loadConfig(s.ConfigPath)
	if err != nil {
		return err
	}
	r, err := buildRouter(cfg, getenv)
	if err != nil {
		return err
	}
	pool, err := openStore(s.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	rdb, err := openRedis(s.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	metrics := telemetry.NewMetrics(r.CircuitStates)

	// As portas são abertas antes de anunciar os endereços, para uma porta ocupada ser
	// um erro na subida. Usa context.Background porque o ctx só significa "encerrar".
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("abrindo porta: %w", err)
	}
	mln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", s.MetricsAddr)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("abrindo porta de métricas: %w", err)
	}
	slog.Info("gateway no ar", "addr", ln.Addr().String(), "metrics_addr", mln.Addr().String())

	handler := api.New(api.Options{
		Router:  r,
		Prices:  cfg.Prices,
		Keys:    store.New(pool),
		Limiter: ratelimit.New(rdb),
		DefaultLimits: ratelimit.Limits{
			RequestsPerMinute: cfg.RateLimit.RequestsPerMinute,
			TokensPerMinute:   cfg.RateLimit.TokensPerMinute,
		},
		// O mesmo Redis do rate limit: os dois guardam dados que se reconstroem, e
		// um segundo Redis seria mais uma peça para operar sem ganho.
		Cache:   cache.New(rdb, cfg.CacheTTL),
		Metrics: metrics,
	}).Handler()

	return serveAll(ctx, []listener{{ln, handler}, {mln, metrics.Handler()}})
}

type listener struct {
	ln      net.Listener
	handler http.Handler
}

// serveAll atende em todas as portas até o ctx ser cancelado. Se uma falhar, as
// outras são encerradas: um gateway que perdeu a porta das métricas continuaria
// atendendo sem ninguém conseguir observá-lo.
func serveAll(ctx context.Context, listeners []listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() { errs <- serve(ctx, l.ln, l.handler) }()
	}

	var failures []error
	for range listeners {
		if err := <-errs; err != nil {
			failures = append(failures, err)
			cancel()
		}
	}
	return errors.Join(failures...)
}

const keysUsage = "uso: gateway keys create -name NOME [-rpm N] [-tpm N] | gateway keys revoke -id ID"

// runKeys administra chaves pela linha de comando. Criar uma chave é raro e feito
// por quem opera o gateway, e uma interface de administração está fora de escopo.
func runKeys(args []string, s settings, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(keysUsage)
	}

	fs := flag.NewFlagSet("keys "+args[0], flag.ContinueOnError)
	fs.SetOutput(stdout)
	name := fs.String("name", "", "quem vai usar a chave")
	id := fs.Int64("id", 0, "id da chave a revogar")
	rpm := fs.Int("rpm", 0, "pedidos por minuto; 0 usa o padrão da config")
	tpm := fs.Int("tpm", 0, "tokens por minuto; 0 usa o padrão da config")
	// As flags são lidas antes de abrir o banco, para um erro de uso não precisar
	// de um Postgres de pé.
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch {
	case args[0] == "create" && *name == "":
		return errors.New("keys create: -name é obrigatório")
	case args[0] == "revoke" && *id <= 0:
		return errors.New("keys revoke: -id é obrigatório")
	case args[0] != "create" && args[0] != "revoke":
		return errors.New(keysUsage)
	}

	pool, err := openStore(s.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool)

	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()

	if args[0] == "create" {
		secret, key, err := st.CreateKey(ctx, *name, store.Limits{RequestsPerMinute: *rpm, TokensPerMinute: *tpm})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "id=%d name=%s\nkey=%s\nGuarde a chave agora: ela não pode ser mostrada de novo.\n", key.ID, key.Name, secret)
		return err
	}

	if err := st.RevokeKey(ctx, *id); err != nil {
		if errors.Is(err, store.ErrKeyNotFound) {
			return fmt.Errorf("keys revoke: a chave %d não existe ou já foi revogada", *id)
		}
		return err
	}
	_, err = fmt.Fprintf(stdout, "chave %d revogada\n", *id)
	return err
}

func main() {
	// SIGTERM é o sinal que o Kubernetes envia antes de encerrar um pod.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n", err)
		os.Exit(1)
	}
}
