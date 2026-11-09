-- Funções comuns aos dois scripts. O Redis não deixa um script importar outro,
-- então o Go concatena este trecho no começo de cada um.
--
-- Um balde guarda o saldo e o instante da última conta, e não um contador zerado
-- a cada minuto: a recarga é calculada sob demanda, e não há nenhum processo
-- recarregando baldes em segundo plano.

-- O relógio é o do Redis, e não o do gateway: réplicas com relógios diferentes
-- calculariam recargas diferentes para o mesmo balde.
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2]) -- microssegundos

local MINUTE_US = 60000000

-- load devolve o saldo atual, já com a recarga desde a última conta. Um balde
-- que não existe está cheio.
local function load(key, capacity)
  local v = redis.call('HMGET', key, 'tokens', 'ts')
  local tokens, ts = tonumber(v[1]), tonumber(v[2])
  if tokens == nil then
    return capacity
  end
  return math.min(capacity, tokens + (now - ts) * capacity / MINUTE_US)
end

-- save grava o saldo e faz a chave expirar quando o balde estaria cheio de novo:
-- um balde cheio é igual a um que não existe, e chaves ociosas não ocupam memória.
local function save(key, tokens, capacity)
  redis.call('HSET', key, 'tokens', tokens, 'ts', now)
  local full_in_ms = math.ceil((capacity - tokens) * 60000 / capacity)
  redis.call('PEXPIRE', key, full_in_ms + 1000)
end

-- wait_ms é quanto falta, em milissegundos, para o saldo chegar a cost.
local function wait_ms(tokens, cost, capacity)
  if tokens >= cost then
    return 0
  end
  return math.ceil((cost - tokens) * 60000 / capacity)
end
