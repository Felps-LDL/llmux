// Package router escolhe o provedor de cada pedido e faz fallback entre eles.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
)

// ErrUnknownModel indica um modelo lógico sem rotas configuradas.
var ErrUnknownModel = errors.New("router: modelo desconhecido")

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

// Router resolve modelos lógicos para listas ordenadas de rotas. Não muda depois
// de criado, então pode atender pedidos concorrentes sem trava.
type Router struct {
	routes         map[string][]Route
	attemptTimeout time.Duration
}

// New cria um Router. attemptTimeout limita cada tentativa, não o pedido inteiro,
// e precisa ser positivo.
func New(routes map[string][]Route, attemptTimeout time.Duration) *Router {
	return &Router{routes: routes, attemptTimeout: attemptTimeout}
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

		req.Model = route.Model
		resp, err := r.attempt(ctx, route, req)
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

func shouldFallback(err error) bool {
	return errors.Is(err, provider.ErrRateLimited) ||
		errors.Is(err, provider.ErrUnavailable) ||
		errors.Is(err, provider.ErrTimeout)
}
