-- KEYS[1]: balde de tokens.
-- ARGV[1]: tokens por minuto; ARGV[2]: tokens a descontar além da reserva
-- (negativo devolve a sobra).
--
-- O saldo pode ficar negativo: se o uso real passou da reserva, a resposta já foi
-- dada, e a dívida é paga pelos próximos pedidos, que esperam a recarga.

local cap, delta = tonumber(ARGV[1]), tonumber(ARGV[2])

-- O teto aqui é redundante com o de load, que limita toda leitura; fica para o
-- valor gravado nunca passar da capacidade, o que deixaria o TTL de save negativo.
local tok = math.min(cap, load(KEYS[1], cap) - delta)
save(KEYS[1], tok, cap)
return math.floor(tok)
