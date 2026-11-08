package store

// MigrateFS expõe migrate só para os testes, que precisam de migrations próprias
// para reproduzir um deploy com uma versão nova.
var MigrateFS = migrate
