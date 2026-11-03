// Package router escolhe o provedor de cada pedido e faz fallback entre eles.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
)

var (
	// ErrUnknownModel indica um modelo lógico sem rotas configuradas.
	ErrUnknownModel = errors.New("router: modelo desconhecido")
	// ErrCircuitOpen indica um provedor pulado porque o circuito dele está aberto.
	ErrCircuitOpen = errors.New("router: circuito aberto")
)

// BreakerConfig define quando o circuito de um provedor abre e por quanto tempo.
type BreakerConfig struct {
	Threshold int
	Cooldown  time.Duration
}

// Route liga um provedor ao nome físico do modelo nele.
type Route struct {
	Provider provider.Provider
	Model    string
}

// Result é uma resposta com o nome do provedor que a produziu.
type Result struct {
	Provider string
	Response *provider.ChatResponse
}

// Router resolve modelos lógicos para listas ordenadas de rotas. Os mapas não
// mudam depois de criados e cada breaker tem a própria trava, então pode atender
// pedidos concorrentes.
type Router struct {
	routes         map[string][]Route
	breakers       map[string]*breaker
	attemptTimeout time.Duration
}

// New cria um Router. attemptTimeout limita cada tentativa, não o pedido inteiro,
// e precisa ser positivo. O nome de cada provedor precisa ser único: rotas com o
// mesmo nome dividem um breaker, porque uma queda afeta todos os modelos dele.
func New(routes map[string][]Route, attemptTimeout time.Duration, cfg BreakerConfig) *Router {
	breakers := make(map[string]*breaker)
	for _, modelRoutes := range routes {
		for _, route := range modelRoutes {
			name := route.Provider.Name()
			if _, ok := breakers[name]; !ok {
				breakers[name] = newBreaker(cfg.Threshold, cfg.Cooldown, time.Now)
			}
		}
	}
	return &Router{routes: routes, breakers: breakers, attemptTimeout: attemptTimeout}
}

// Chat atende req pelo modelo lógico model, tentando as rotas em ordem. O campo
// req.Model é ignorado: cada rota define o nome físico.
func (r *Router) Chat(ctx context.Context, model string, req provider.ChatRequest) (*Result, error) {
	routes := r.routes[model]
	if len(routes) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, model)
	}

	var failures []error
	for _, route := range routes {
		// Com o cliente desconectado ou o prazo dele esgotado, a próxima tentativa
		// falharia na hora ou pagaria por uma resposta que ninguém vai ler.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("router: modelo %q: %w", model, err)
		}

		br := r.breakers[route.Provider.Name()]
		if !br.allow() {
			failures = append(failures, fmt.Errorf("%s: %w", route.Provider.Name(), ErrCircuitOpen))
			continue
		}

		req.Model = route.Model
		resp, err := r.attempt(ctx, route, req)
		record(ctx, br, err)
		if err == nil {
			return &Result{Provider: route.Provider.Name(), Response: resp}, nil
		}

		err = fmt.Errorf("%s: %w", route.Provider.Name(), err)
		if !shouldFallback(err) {
			return nil, fmt.Errorf("router: modelo %q: %w", model, err)
		}
		failures = append(failures, err)
	}

	return nil, fmt.Errorf("router: modelo %q: todos os provedores falharam: %w", model, errors.Join(failures...))
}

// attempt existe para o defer liberar o timer a cada tentativa; dentro do loop de
// Chat, ele só rodaria ao fim do pedido.
func (r *Router) attempt(ctx context.Context, route Route, req provider.ChatRequest) (*provider.ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, r.attemptTimeout)
	defer cancel()

	return route.Provider.Chat(ctx, req)
}

// record informa ao breaker o resultado de uma tentativa. O ctx é o do pedido, não
// o da tentativa: se o prazo do cliente acabou, a falha não diz nada sobre o
// provedor, e clientes com prazo curto abririam o circuito de um provedor saudável.
func record(ctx context.Context, br *breaker, err error) {
	switch {
	case err == nil:
		br.success()
	case ctx.Err() != nil:
		br.release()
	// O 429 fica de fora: o provedor está vivo e só limitando este cliente.
	case errors.Is(err, provider.ErrUnavailable), errors.Is(err, provider.ErrTimeout):
		br.failure()
	default:
		br.release()
	}
}

func shouldFallback(err error) bool {
	return errors.Is(err, provider.ErrRateLimited) ||
		errors.Is(err, provider.ErrUnavailable) ||
		errors.Is(err, provider.ErrTimeout)
}
