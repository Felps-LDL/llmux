-- +goose Up
-- A chave em si nunca é guardada, só o SHA-256 dela: se o banco vazar, as chaves
-- não vazam junto. O UNIQUE cria o índice que a busca de cada pedido usa.
CREATE TABLE api_keys (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL,
    key_hash   bytea       NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Revogar em vez de apagar mantém o histórico de quem a chave era, que o
    -- registro de uso por chave vai precisar.
    revoked_at timestamptz
);

-- +goose Down
DROP TABLE api_keys;
