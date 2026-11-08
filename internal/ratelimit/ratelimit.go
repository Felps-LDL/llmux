// Package ratelimit limita pedidos e tokens por minuto de cada chave de API, com
// token buckets no Redis compartilhados por todas as réplicas do gateway.
package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	//go:embed bucket.lua
	bucketLua string
	//go:embed reserve.lua
	reserveLua string
	//go:embed settle.lua
	settleLua string

	// NewScript manda só o hash do script (EVALSHA) e o texto inteiro apenas na
	// primeira vez que o Redis não o conhece.
	reserveScript = redis.NewScript(bucketLua + reserveLua)
	settleScript  = redis.NewScript(bucketLua + settleLua)
)

// Limits são os limites por minuto de uma chave. Os dois precisam ser positivos.
type Limits struct {
	RequestsPerMinute int
	TokensPerMinute   int
}

// Decision é o resultado de Reserve.
type Decision struct {
	Allowed bool
	// RetryAfter é quanto esperar até o pedido caber, quando Allowed é false.
	RetryAfter time.Duration
	// Reserved é quanto foi reservado, que pode ser menos que o pedido (ver
	// Reserve). É o valor que Settle precisa receber.
	Reserved int
	// Os saldos depois da decisão, para os cabeçalhos x-ratelimit-remaining-*.
	// Nunca negativos: uma dívida aparece como zero.
	RemainingRequests int
	RemainingTokens   int
}

// Limiter aplica os limites. É seguro para uso concorrente.
type Limiter struct {
	rdb redis.Scripter
}

// New cria um Limiter sobre um cliente do Redis.
func New(rdb redis.Scripter) *Limiter {
	return &Limiter{rdb: rdb}
}

// As duas chaves de um script precisam estar no mesmo nó de um Redis Cluster; a
// parte entre chaves ({id}) é a única que decide o nó.
func requestsKey(keyID int64) string { return fmt.Sprintf("llmux:rl:{%d}:req", keyID) }
func tokensKey(keyID int64) string   { return fmt.Sprintf("llmux:rl:{%d}:tok", keyID) }

// Reserve tenta reservar um pedido e tokens da chave keyID. Uma reserva maior que
// o limite por minuto é reduzida a ele: senão o pedido nunca caberia, nem com o
// balde cheio. O excesso vira dívida no Settle.
func (l *Limiter) Reserve(ctx context.Context, keyID int64, limits Limits, tokens int) (Decision, error) {
	tokens = max(1, min(tokens, limits.TokensPerMinute))

	res, err := reserveScript.Run(ctx, l.rdb,
		[]string{requestsKey(keyID), tokensKey(keyID)},
		limits.RequestsPerMinute, limits.TokensPerMinute, tokens,
	).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: reservando: %w", err)
	}
	if len(res) != 4 {
		return Decision{}, fmt.Errorf("ratelimit: resposta inesperada do script: %v", res)
	}

	return Decision{
		Allowed:           res[0] == 1,
		RetryAfter:        time.Duration(res[1]) * time.Millisecond,
		Reserved:          tokens,
		RemainingRequests: int(max(0, res[2])),
		RemainingTokens:   int(max(0, res[3])),
	}, nil
}

// Settle acerta uma reserva com o uso real: devolve a sobra, ou desconta o que
// passou dela. reserved é o Decision.Reserved, e used é o total de tokens que o
// provedor informou; com used zero, a reserva volta inteira.
func (l *Limiter) Settle(ctx context.Context, keyID int64, limits Limits, reserved, used int) error {
	delta := used - reserved
	if delta == 0 {
		return nil
	}
	err := settleScript.Run(ctx, l.rdb, []string{tokensKey(keyID)}, limits.TokensPerMinute, delta).Err()
	if err != nil {
		return fmt.Errorf("ratelimit: acertando: %w", err)
	}
	return nil
}
