package provider

import "errors"

// Classes de falha que um adaptador deve embrulhar com %w. Elas existem para o
// router decidir o fallback sem conhecer status HTTP nem erros de cada SDK.
//
// Qualquer outro erro fica sem classe, de propósito. Em especial,
// context.Canceled: o cliente desistiu, e tentar outro provedor pagaria por uma
// resposta que ninguém vai ler.
var (
	// ErrRateLimited indica que o provedor recusou o pedido por limite (HTTP 429).
	ErrRateLimited = errors.New("provedor: limite excedido")
	// ErrUnavailable indica resposta 5xx ou falha de conexão com o provedor.
	ErrUnavailable = errors.New("provedor: indisponível")
	// ErrTimeout indica que o provedor não respondeu dentro do prazo.
	ErrTimeout = errors.New("provedor: tempo esgotado")
)
