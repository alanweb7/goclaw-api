# goclaw-api

API HTTP em Go que faz ponte para o WebSocket do GoClaw (`wss://ws.geoclaw.pullse.ia.br/ws`).

## Requisitos

- Go 1.23+
- Token do gateway GoClaw

## Variáveis de ambiente

- `GOCLAW_TOKEN` (obrigatória)
- `GOCLAW_WS_URL` (opcional, padrão: `wss://ws.geoclaw.pullse.ia.br/ws`)
- `PORT` (opcional, padrão: `8080`)

## Rodar

```bash
go mod tidy
go run .
```

## Endpoints

- `GET /health`
- `POST /v1/chat`

Payload de `POST /v1/chat`:

```json
{
  "user_id": "alanweb7",
  "agent_id": "luna-clara",
  "session_key": "agent:luna-clara:ws:direct:1234",
  "message": "A partir de agora, meu nome é Alan.",
  "stream": true,
  "locale": "pt-BR"
}
```

Exemplo:

```bash
curl --location 'http://localhost:8080/v1/chat' \
  --header 'Content-Type: application/json' \
  --data '{
    "user_id":"alanweb7",
    "agent_id":"luna-clara",
    "session_key":"agent:luna-clara:ws:direct:1234",
    "message":"Meu nome é Alan. Qual é meu nome?",
    "stream":true
  }'
```

Se você repetir o mesmo `session_key`, a conversa continua na mesma sessão no GoClaw.
