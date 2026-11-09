-- +goose Up
-- NULL significa "use o padrão da config": mudar o padrão é mudar a config, sem
-- migrar dados. Colunas sem default só alteram o catálogo, sem reescrever a tabela,
-- então a migration é segura numa tabela grande.
ALTER TABLE api_keys
    ADD COLUMN requests_per_minute integer CHECK (requests_per_minute > 0),
    ADD COLUMN tokens_per_minute   integer CHECK (tokens_per_minute > 0);

-- +goose Down
ALTER TABLE api_keys
    DROP COLUMN requests_per_minute,
    DROP COLUMN tokens_per_minute;
