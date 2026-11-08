-- KEYS[1]: balde de pedidos; KEYS[2]: balde de tokens.
-- ARGV[1]: pedidos por minuto; ARGV[2]: tokens por minuto; ARGV[3]: tokens a reservar.
-- Devolve {permitido (1/0), espera em ms, pedidos restantes, tokens restantes}.
--
-- Os dois baldes são decididos juntos, num script só: se o de tokens recusa, o de
-- pedidos não pode ter sido descontado. E o script roda inteiro sem nenhum outro
-- comando no meio, então duas réplicas nunca leem o mesmo saldo.

local req_cap, tok_cap, cost = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])

local req = load(KEYS[1], req_cap)
local tok = load(KEYS[2], tok_cap)

local wait = math.max(wait_ms(req, 1, req_cap), wait_ms(tok, cost, tok_cap))
if wait > 0 then
  -- Recusado: nada é gravado, e a recarga continua contando da última conta.
  return {0, wait, math.floor(req), math.floor(tok)}
end

req = req - 1
tok = tok - cost
save(KEYS[1], req, req_cap)
save(KEYS[2], tok, tok_cap)
return {1, 0, math.floor(req), math.floor(tok)}
