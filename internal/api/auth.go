package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/Felps-LDL/llmux/internal/store"
)

// KeyLookup encontra a chave de um segredo. É a interface de que a API precisa, e
// não o Store inteiro: assim os testes da API usam um falso, sem banco, como o
// router usa o provedor falso.
type KeyLookup interface {
	LookupKey(ctx context.Context, secret string) (store.APIKey, error)
}

// authenticate exige uma chave de API válida em Authorization: Bearer, o cabeçalho
// que os SDKs da OpenAI mandam com a api_key.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := bearerToken(r)
		if !ok {
			writeUnauthorized(w)
			return
		}

		key, err := s.keys.LookupKey(r.Context(), secret)
		switch {
		case errors.Is(err, store.ErrKeyNotFound):
			writeUnauthorized(w)
		case err != nil:
			// O cliente já foi embora: não há para quem responder.
			if r.Context().Err() != nil {
				return
			}
			// 503, e não 401: um 401 diria ao cliente que a chave é inválida, e ele
			// poderia descartá-la por causa de um banco fora do ar. O detalhe fica no
			// log; o segredo nunca vai para ele.
			log.Printf("api: verificando chave: %v", err)
			writeError(w, http.StatusServiceUnavailable, "server_error", "não foi possível verificar a chave de API")
		default:
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyCtxKey{}, key)))
		}
	})
}

// keyCtxKey é um tipo próprio, e não uma string, para nenhum outro pacote colidir
// com a mesma chave de contexto por acaso.
type keyCtxKey struct{}

// keyFrom devolve a chave que authenticate validou. Só é chamada por handlers
// atrás do middleware, então a chave sempre está lá.
func keyFrom(ctx context.Context) store.APIKey {
	key, _ := ctx.Value(keyCtxKey{}).(store.APIKey)
	return key
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	// O esquema não diferencia maiúsculas de minúsculas (RFC 7235).
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// writeUnauthorized dá a mesma resposta para chave ausente, inexistente ou
// revogada: quem testa chaves não aprende nada com a diferença.
func writeUnauthorized(w http.ResponseWriter) {
	// A RFC 6750 pede este cabeçalho num 401, para o cliente saber o esquema
	// esperado.
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "invalid_request_error", "chave de API ausente ou inválida")
}
