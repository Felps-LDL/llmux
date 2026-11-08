package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrKeyNotFound indica uma chave inexistente ou revogada. As duas são o mesmo
// erro de propósito: quem tenta chaves não deve saber quais já existiram.
var ErrKeyNotFound = errors.New("store: chave não encontrada")

// O prefixo deixa uma chave vazada reconhecível num log ou por um scanner de
// segredos, como fazem os provedores com sk- e sk-ant-.
const keyPrefix = "llmux_"

// Limits são os limites de uma chave. Zero significa "use o padrão da config".
type Limits struct {
	RequestsPerMinute int
	TokensPerMinute   int
}

// APIKey é uma chave de API, sem o segredo: ele só existe na resposta de CreateKey.
type APIKey struct {
	ID        int64
	Name      string
	CreatedAt time.Time
	Limits    Limits
}

// Store acessa o Postgres. O pool é seguro para uso concorrente.
type Store struct {
	pool *pgxpool.Pool
}

// New cria um Store sobre um pool já conectado e migrado.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// hashKey usa SHA-256, e não bcrypt como numa senha: a chave tem 130 bits
// aleatórios, então força bruta é inviável mesmo com hash rápido. E um hash sem
// salt deixa achar a chave pelo índice em cada pedido, o que o bcrypt impediria.
func hashKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// CreateKey cria uma chave e devolve o segredo. É a única vez que ele aparece:
// o banco guarda só o hash, e uma chave perdida precisa ser trocada por outra.
func (s *Store) CreateKey(ctx context.Context, name string, limits Limits) (string, APIKey, error) {
	secret := keyPrefix + rand.Text()

	key := APIKey{Name: name, Limits: limits}
	// NULLIF guarda o zero como NULL, que é o "padrão da config" na tabela. Um limite
	// negativo é recusado pelo CHECK da tabela, que vale para qualquer escrita, e
	// não só para as que passam por aqui.
	err := s.pool.QueryRow(ctx,
		`INSERT INTO api_keys (name, key_hash, requests_per_minute, tokens_per_minute)
		 VALUES ($1, $2, NULLIF($3, 0), NULLIF($4, 0)) RETURNING id, created_at`,
		name, hashKey(secret), limits.RequestsPerMinute, limits.TokensPerMinute,
	).Scan(&key.ID, &key.CreatedAt)
	if err != nil {
		return "", APIKey{}, fmt.Errorf("store: criando chave: %w", err)
	}
	return secret, key, nil
}

// LookupKey encontra a chave ativa de um segredo.
func (s *Store) LookupKey(ctx context.Context, secret string) (APIKey, error) {
	var key APIKey
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, created_at, COALESCE(requests_per_minute, 0), COALESCE(tokens_per_minute, 0)
		 FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
		hashKey(secret),
	).Scan(&key.ID, &key.Name, &key.CreatedAt, &key.Limits.RequestsPerMinute, &key.Limits.TokensPerMinute)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ErrKeyNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("store: buscando chave: %w", err)
	}
	return key, nil
}

// RevokeKey revoga uma chave. Revogar uma chave já revogada é ErrKeyNotFound, para
// quem chama saber que nada mudou.
func (s *Store) RevokeKey(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("store: revogando chave: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}
