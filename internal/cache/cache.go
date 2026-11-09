// Package cache guarda respostas no Redis pelo hash exato do pedido.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Felps-LDL/llmux/internal/provider"
)

// Entry é uma resposta guardada, com o provedor e o modelo físico da rota que a
// produziu, para o acerto devolver os mesmos cabeçalhos da resposta original.
type Entry struct {
	Provider string
	Model    string
	Response provider.ChatResponse
}

// Cache lê e grava respostas. É seguro para uso concorrente.
type Cache struct {
	rdb redis.Cmdable
	ttl time.Duration
}

// New cria um Cache. ttl precisa ser positivo: respostas envelhecem, e um modelo
// atualizado no provedor responderia diferente.
func New(rdb redis.Cmdable, ttl time.Duration) *Cache {
	return &Cache{rdb: rdb, ttl: ttl}
}

// keyMaterial é o que entra no hash. O pedido vai inteiro, e não campo a campo:
// um campo acrescentado a ChatRequest no futuro entra na chave sozinho, em vez de
// ser esquecido e fazer pedidos diferentes acertarem a mesma resposta.
type keyMaterial struct {
	// O cache é separado por chave de API: compartilhado, o custo zero de um acerto
	// revelaria que outro cliente já fez exatamente a mesma pergunta.
	APIKeyID int64
	Model    string
	Request  provider.ChatRequest
}

// Key é a chave do cache para um pedido. model é o nome lógico, porque o físico
// só é conhecido depois de o router escolher a rota.
func Key(apiKeyID int64, model string, req provider.ChatRequest) (string, error) {
	// O JSON de uma struct sai sempre na mesma ordem de campos, então o mesmo
	// pedido dá sempre os mesmos bytes, e o mesmo hash.
	data, err := json.Marshal(keyMaterial{APIKeyID: apiKeyID, Model: model, Request: req})
	if err != nil {
		return "", fmt.Errorf("cache: montando a chave: %w", err)
	}
	sum := sha256.Sum256(data)
	return "llmux:cache:" + hex.EncodeToString(sum[:]), nil
}

// Get devolve a resposta guardada em key; ok é false se não houver.
func (c *Cache) Get(ctx context.Context, key string) (entry Entry, ok bool, err error) {
	data, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("cache: lendo: %w", err)
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, false, fmt.Errorf("cache: decodificando: %w", err)
	}
	return entry, true, nil
}

// Set guarda uma resposta em key pelo TTL do Cache.
func (c *Cache) Set(ctx context.Context, key string, entry Entry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("cache: codificando: %w", err)
	}
	if err := c.rdb.Set(ctx, key, data, c.ttl).Err(); err != nil {
		return fmt.Errorf("cache: gravando: %w", err)
	}
	return nil
}
