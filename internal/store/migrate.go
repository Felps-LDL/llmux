// Package store guarda o estado do gateway no Postgres.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// As migrations vão dentro do binário: a imagem distroless não tem os arquivos, e
// o binário nunca roda com um esquema de outra versão.
//
//go:embed migrations/*.sql
var migrations embed.FS

// Migrate aplica as migrations pendentes. Pode rodar em várias réplicas ao mesmo
// tempo: uma trava do próprio Postgres (advisory lock) faz uma esperar a outra.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("lendo migrations: %w", err)
	}
	return migrate(ctx, pool, fsys)
}

// migrate recebe as migrations como parâmetro para o teste de concorrência poder
// usar as próprias.
func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	// O padrão do goose tenta de novo a cada 5s: com 5 réplicas na fila, a última
	// esperaria uns 20s sem motivo. A cada 1s, mantendo o limite total de 5 min.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return fmt.Errorf("criando trava: %w", err)
	}

	// O goose fala database/sql. Fechar este adaptador não fecha o pool, que
	// continua sendo de quem o criou.
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	p, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("preparando migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("aplicando migrations: %w", err)
	}
	return nil
}
