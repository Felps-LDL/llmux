package store_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Felps-LDL/llmux/internal/store"
)

// A mesma imagem do docker-compose: um teste contra outra versão do Postgres
// poderia passar aqui e falhar no ambiente de verdade.
const postgresImage = "postgres:18-alpine"

var (
	container *postgres.PostgresContainer
	pool      *pgxpool.Pool
)

// TestMain sobe um Postgres para o pacote inteiro. Um container por teste custaria
// segundos em cada um; em troca, os testes não rodam em paralelo e cada um começa
// limpando as tabelas (ver reset).
func TestMain(m *testing.M) {
	ctx := context.Background()

	var err error
	container, err = postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("llmux"),
		postgres.WithUsername("llmux"),
		postgres.WithPassword("llmux"),
		// Sem esperar, o teste conectaria antes de o banco aceitar conexões, o mesmo
		// problema que o healthcheck resolve no compose.
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		log.Fatalf("subindo o Postgres: %v", err)
	}

	pool, err = connect(ctx, "llmux")
	if err != nil {
		log.Fatalf("conectando: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrando: %v", err)
	}

	code := m.Run()

	pool.Close()
	if err := testcontainers.TerminateContainer(container); err != nil {
		log.Printf("derrubando o Postgres: %v", err)
	}
	os.Exit(code)
}

// connect abre um pool para um banco do container. Os testes de migration usam
// bancos próprios, para começarem sem nenhuma tabela.
func connect(ctx context.Context, database string) (*pgxpool.Pool, error) {
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, err
	}
	dsn = strings.Replace(dsn, "/llmux?", "/"+database+"?", 1)
	return pgxpool.New(ctx, dsn)
}

// reset deixa a tabela vazia. É chamado no começo de cada teste, e não no fim,
// para um teste que falhou no meio não estragar o seguinte.
func reset(t *testing.T) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `TRUNCATE api_keys RESTART IDENTITY`); err != nil {
		t.Fatalf("limpando api_keys: %v", err)
	}
}

// Verifica o caminho principal e a promessa de segurança da tabela. Encontrar a
// chave pelo segredo é o que a autenticação vai fazer em cada pedido; procurar o
// segredo dentro da linha gravada prova que o banco guarda só o hash, e que um
// vazamento do banco não entregaria as chaves.
func TestCriaEEncontraChaveSemGuardarOSegredo(t *testing.T) {
	reset(t)
	s := store.New(pool)

	secret, created, err := s.CreateKey(t.Context(), "cliente-a")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if !strings.HasPrefix(secret, "llmux_") {
		t.Errorf("segredo = %q, want o prefixo llmux_", secret)
	}

	got, err := s.LookupKey(t.Context(), secret)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	if got.ID != created.ID || got.Name != "cliente-a" {
		t.Errorf("LookupKey = %+v, want a chave criada (%+v)", got, created)
	}

	// A linha inteira como texto: se o segredo aparecer em qualquer coluna, inclusive
	// numa que alguém acrescente depois, o teste pega.
	var row string
	if err := pool.QueryRow(t.Context(), `SELECT k::text FROM api_keys k WHERE id = $1`, created.ID).Scan(&row); err != nil {
		t.Fatalf("lendo a linha: %v", err)
	}
	if strings.Contains(row, secret) || strings.Contains(row, strings.TrimPrefix(secret, "llmux_")) {
		t.Errorf("a linha gravada contém o segredo: %s", row)
	}
}

// Verifica a trava das migrations, que só existe sob concorrência. O cenário é um
// deploy: o banco já está na versão 1, e várias réplicas sobem juntas com uma
// versão 2. Sem a trava, todas tentam criar a mesma tabela e só uma consegue.
//
// Num banco vazio este teste não provaria nada: a criação da tabela de controle do
// goose já faz as réplicas esperarem umas pelas outras, com ou sem trava. Foi
// medido assim, e é por isso que o banco começa na versão 1.
func TestMigrateAguentaReplicasSubindoJuntas(t *testing.T) {
	ctx := t.Context()
	// Nome próprio por execução, para o teste poder rodar várias vezes seguidas
	// (go test -count): corridas aparecem por probabilidade, não toda vez.
	name := "replicas_" + strings.ToLower(rand.Text()[:8])
	if _, err := pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("criando banco: %v", err)
	}
	fresh, err := connect(ctx, name)
	if err != nil {
		t.Fatalf("conectando: %v", err)
	}
	defer fresh.Close()

	v1 := fstest.MapFS{"00001_a.sql": {Data: []byte("-- +goose Up\nCREATE TABLE a (x int);\n")}}
	if err := store.MigrateFS(ctx, fresh, v1); err != nil {
		t.Fatalf("deploy anterior: %v", err)
	}

	v2 := fstest.MapFS{
		"00001_a.sql": v1["00001_a.sql"],
		// O pg_sleep alarga a janela da corrida: sem ele, uma réplica poderia
		// terminar antes de a outra começar, e o teste passaria sem a trava.
		"00002_b.sql": {Data: []byte("-- +goose Up\nSELECT pg_sleep(0.3);\nCREATE TABLE b (x int);\n")},
	}

	const replicas = 5
	errs := make([]error, replicas)
	// Fechar o canal solta todas juntas, como no teste de concorrência do breaker.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Go(func() {
			<-start
			errs[i] = store.MigrateFS(ctx, fresh, v2)
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("réplica %d: %v", i, err)
		}
	}
	// Todas sem erro não basta: a versão 2 precisa ter sido aplicada uma vez só.
	var applied int
	if err := fresh.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id = 2`).Scan(&applied); err != nil {
		t.Fatalf("lendo versões: %v", err)
	}
	if applied != 1 {
		t.Errorf("versão 2 registrada %d vezes, want 1", applied)
	}
}

func TestChaveRevogadaNaoEEncontrada(t *testing.T) {
	reset(t)
	s := store.New(pool)

	secret, key, err := s.CreateKey(t.Context(), "cliente-a")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := s.RevokeKey(t.Context(), key.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	if _, err := s.LookupKey(t.Context(), secret); !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("LookupKey após revogar: erro = %v, want %v", err, store.ErrKeyNotFound)
	}
	// Revogar de novo não muda nada, e quem chama precisa saber disso.
	if err := s.RevokeKey(t.Context(), key.ID); !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("RevokeKey de novo: erro = %v, want %v", err, store.ErrKeyNotFound)
	}
}

func TestChavesSaoUnicasEIndependentes(t *testing.T) {
	reset(t)
	s := store.New(pool)

	var secrets []string
	var keys []store.APIKey
	for i := range 3 {
		secret, key, err := s.CreateKey(t.Context(), fmt.Sprintf("cliente-%d", i))
		if err != nil {
			t.Fatalf("CreateKey %d: %v", i, err)
		}
		secrets = append(secrets, secret)
		keys = append(keys, key)
	}
	if secrets[0] == secrets[1] || secrets[1] == secrets[2] || secrets[0] == secrets[2] {
		t.Fatalf("segredos repetidos: %q", secrets)
	}

	// Revogar a do meio não pode afetar as vizinhas: um WHERE errado no UPDATE
	// revogaria todas, ou a errada.
	if err := s.RevokeKey(t.Context(), keys[1].ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	for i, secret := range secrets {
		got, err := s.LookupKey(t.Context(), secret)
		if i == 1 {
			if !errors.Is(err, store.ErrKeyNotFound) {
				t.Errorf("chave revogada: erro = %v, want %v", err, store.ErrKeyNotFound)
			}
			continue
		}
		if err != nil || got.ID != keys[i].ID {
			t.Errorf("chave %d: LookupKey = (%+v, %v), want a chave %d", i, got, err, keys[i].ID)
		}
	}
}

func TestSegredoDesconhecidoNaoEEncontrado(t *testing.T) {
	reset(t)
	s := store.New(pool)

	if _, _, err := s.CreateKey(t.Context(), "cliente-a"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := s.LookupKey(t.Context(), "llmux_inventada"); !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("erro = %v, want %v", err, store.ErrKeyNotFound)
	}
}
