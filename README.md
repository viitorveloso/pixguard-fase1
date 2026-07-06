# pixledger — Core Payment Ledger API

![Go](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-4169E1?logo=postgresql&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-compose-2496ED?logo=docker&logoColor=white)
![CI](https://img.shields.io/badge/CI-GitHub_Actions-2088FF?logo=githubactions&logoColor=white)
![Tests](https://img.shields.io/badge/tests-race_detector-brightgreen)

Ledger de pagamentos estilo Pix em Go: contas, saldos em centavos e transferências com **idempotência atômica**, **locks ordenados sem deadlock** e **outbox transacional** para webhooks. Stdlib-first — apenas duas dependências (`lib/pq` e `google/uuid`).

> **Nota:** o module path é `pixledger`. Ao publicar, renomeie no `go.mod` para `github.com/SEU_USUARIO/pixledger` e ajuste os imports (find/replace de `pixledger/` para `github.com/SEU_USUARIO/pixledger/`).

## Arquitetura

```mermaid
flowchart LR
    C[Cliente] -->|Bearer JWT + X-Idempotency-Key| H[HTTP API<br/>net/http 1.22]
    H --> S[Service<br/>validação + orquestração]
    S --> ST[(PostgreSQL<br/>store)]
    ST -->|mesma transação| E[(events<br/>outbox)]
    W[Outbox Worker] -->|FOR UPDATE SKIP LOCKED| E
    W -->|POST + backoff| WH[Webhook]
    H --> M[/metrics<br/>Prometheus/]
```

Camadas: `httpapi` (transporte) → `service` (regras) → `store/postgres` (garantias financeiras). A interface `Store` é definida no consumidor (`service`), com implementação Postgres para produção e uma in-memory que replica a mesma semântica para testes unitários rápidos.

## Stack

| Componente | Escolha | Por quê |
|---|---|---|
| HTTP | `net/http` puro (patterns do Go 1.22) | roteador externo não paga o próprio custo aqui |
| Banco | PostgreSQL 16 + `lib/pq` | `SELECT ... FOR UPDATE`, `ON CONFLICT`, `SKIP LOCKED` |
| Auth | JWT HS256 implementado na stdlib | escopo pequeno, `alg=none` rejeitado, comparação constant-time |
| Métricas | registry próprio, formato de exposição Prometheus | contadores + 1 histograma não justificam a client library |
| Logs | `log/slog` JSON | estruturado, com `request_id` propagado |

## Como rodar

```bash
# Tudo em containers (Postgres + API)
make compose-up

# Ou local: suba um Postgres e rode
make run

# Token de desenvolvimento
make token
# ou: go run ./cmd/tokengen -sub lucas -ttl 24h

# Testes unitários (race detector)
make test

# Testes de integração contra Postgres real
make test-integration
```

Requisições prontas em [`api.http`](api.http) (VS Code REST Client). Variáveis de ambiente em [`.env.example`](.env.example).

## Endpoints

| Método | Rota | Auth | Descrição |
|---|---|---|---|
| `POST` | `/api/accounts` | Bearer | Cria conta (`number`, `holder`, `initial_balance_cents`) |
| `GET` | `/api/accounts/{id}` | Bearer | Consulta conta e saldo |
| `POST` | `/api/pix` | Bearer + `X-Idempotency-Key` | Transferência entre contas |
| `GET` | `/healthz` | — | Liveness |
| `GET` | `/readyz` | — | Readiness (ping no Postgres, timeout 2s) |
| `GET` | `/metrics` | — | Métricas Prometheus |

### Códigos de resposta do `/api/pix`

| Status | `error.code` | Quando |
|---|---|---|
| `201` | — | Transferência executada |
| `200` + header `Idempotent-Replayed: true` | — | Replay da mesma chave com mesmo payload |
| `400` | `missing_idempotency_key` | Sem header `X-Idempotency-Key` |
| `400` | `invalid_payload` | JSON malformado, campo desconhecido, descrição > 140 |
| `402` | `insufficient_funds` | Saldo insuficiente (gravado; replay devolve o mesmo 402 com o mesmo `transaction_id`) |
| `404` | `account_not_found` | Conta origem/destino inexistente |
| `409` | `idempotency_conflict` | Mesma chave com payload diferente |
| `422` | `invalid_amount` / `same_account` | Valor ≤ 0 / origem = destino |
| `401` | `unauthorized` | Token ausente, inválido ou expirado |

## Garantias & decisões de design

**Dinheiro é `int64` em centavos, nunca float.** E o banco é a última linha de defesa: `CHECK (balance_cents >= 0)` e `CHECK (amount_cents > 0)` valem mesmo que a aplicação tenha um bug.

**Idempotência é atômica, na mesma transação da transferência.** O fluxo abre uma transação e faz `INSERT ... ON CONFLICT (key) DO NOTHING RETURNING id`. Quem ganha a corrida executa; quem perde **bloqueia no índice único até o vencedor commitar** e então lê o resultado dele — por isso 30 requisições simultâneas com a mesma chave produzem exatamente 1 débito (provado em teste). Chave igual com payload diferente é detectada por hash SHA-256 do payload → `409`.

**Saldo insuficiente é decisão de negócio, não erro de transporte.** A transferência `failed` é gravada, vinculada à chave e **commitada** — o mesmo `X-Idempotency-Key` devolve o mesmo `402` com o mesmo `transaction_id` para sempre. Erros de validação e conta inexistente fazem rollback e **não** consomem a chave (o cliente corrige e reenvia).

**Deadlock é impossível por construção, não por sorte.** As duas contas são travadas com `SELECT ... FOR UPDATE` **sempre em ordem lexicográfica de ID**. Com ordem global de aquisição (chave primeiro, depois contas ordenadas), não existe ciclo de espera. O retry em `40001`/`40P01` (3 tentativas, backoff) fica como cinto de segurança. O teste de crossfire dispara 500 transferências bidirecionais simultâneas A↔B e exige 100% de sucesso com saldos exatos.

**Outbox transacional: o commit nunca espera a rede.** O evento `transfer.completed` é inserido na **mesma transação** da transferência. Um worker separado varre pendências com `FOR UPDATE SKIP LOCKED` (seguro com múltiplas réplicas), entrega via POST e agenda retries com backoff exponencial + jitter. Entrega é *at-least-once* — consumidores deduplicam pelo `end_to_end_id` / `id` do evento.

**Shutdown gracioso de verdade.** `SIGINT`/`SIGTERM` → `server.Shutdown` com timeout configurável, worker cancelado via contexto, `WaitGroup` espera tudo terminar.

**EndToEndId no padrão Bacen.** `E` + ISPB (8 dígitos) + `yyyyMMddHHmm` + 11 alfanuméricos = 32 chars, sufixo com `crypto/rand` (colisão de identificador financeiro não é aceitável com `math/rand`).

## Testes e o que eles provam

```bash
go test ./... -race -count=1                      # unitários
DATABASE_URL=... go test ./... -race -count=1     # + integração
```

| Teste (integração, Postgres real) | Prova |
|---|---|
| 50 goroutines, chaves distintas, drenando saldo | Exatamente 10×`201` e 40×`402`; saldos finais exatos; 10 linhas `completed` no banco |
| 30 goroutines, **mesma** chave | Exatamente 1×`201` + 29×`200` com header de replay; **1 débito**, 1 linha em `transactions` |
| Crossfire A↔B: 20 goroutines × 25 iterações simultâneas | Zero deadlocks/timeouts; dinheiro conservado; saldos finais idênticos aos iniciais |
| Replay & conflito | Mesmo `transaction_id`/`end_to_end_id` no replay; `409` com payload alterado |
| Saldo insuficiente | `402` persistido como `failed` e replayado deterministicamente |
| Outbox com webhook falhando 2× | Worker retenta com backoff e entrega na 3ª (`attempts == 3`); payload íntegro |

Unitários cobrem domínio (formato do EndToEndId, validações), JWT (roundtrip, expiração, adulteração, `alg=none`), métricas e as mesmas tempestades de concorrência contra o store in-memory.

## Variáveis de ambiente

| Variável | Default | Descrição |
|---|---|---|
| `PORT` | `8080` | Porta HTTP |
| `DATABASE_URL` | `postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable` | Conexão Postgres |
| `JWT_SECRET` | `dev-secret-change-me` | Segredo HS256 (warn no boot se ficar no default) |
| `ISPB` | `00000000` | ISPB usado no EndToEndId |
| `WEBHOOK_URL` | *(vazio)* | Se setado, liga o worker de outbox |
| `OUTBOX_POLL_INTERVAL` | `2s` | Intervalo de varredura |
| `OUTBOX_BASE_BACKOFF` / `OUTBOX_MAX_BACKOFF` | `1s` / `60s` | Backoff exponencial dos retries |
| `SHUTDOWN_TIMEOUT` | `10s` | Prazo do graceful shutdown |
| `LOG_LEVEL` | `info` | `info` ou `debug` |

## Estrutura

```
cmd/api/            entrypoint (config, migração, wiring, shutdown)
cmd/tokengen/       gerador de token de dev
internal/domain/    entidades, erros-sentinela, EndToEndId
internal/service/   regras de negócio + interface Store
internal/store/     postgres (produção) e memory (testes)
internal/httpapi/   rotas, auth, middleware, mapeamento de erros
internal/jwt/       HS256 stdlib
internal/metrics/   registry Prometheus zero-dep
internal/outbox/    worker de entrega de webhooks
internal/integration/ suíte contra Postgres real
migrations/         SQL embarcado (go:embed), aplicado no boot
```

## Trade-offs e próximos passos

- **`lib/pq` → `pgx` + `sqlc`**: pq está em manutenção; pgx tem melhor performance e tipos nativos. sqlc geraria o boilerplate de queries com type-safety.
- **Read model / CQRS**: extrato e consultas de histórico hoje leriam da mesma tabela; um projetor consumindo `events` desacoplaria leitura de escrita.
- **Observabilidade**: trocar o registry próprio por OpenTelemetry (traces + métricas) quando houver mais de um serviço.
- **Rate limiting e mTLS** no gateway (ver [gogate](https://github.com/) 😉) em vez de dentro do serviço.
- **Particionamento de `transactions`** por data quando o volume justificar.
