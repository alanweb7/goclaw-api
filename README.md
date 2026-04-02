# goclaw-api

API HTTP em Go que faz ponte para o WebSocket do GoClaw (`wss://ws.geoclaw.pullse.ia.br/ws`) e permite manter sessao persistente via `session_key`.

## O que esta API resolve

- Expoe endpoint REST (`POST /v1/chat`) para quem nao quer falar WebSocket direto.
- Internamente conecta no GoClaw via WS, faz `connect` e `chat.send`.
- Mantem contexto da conversa quando voce reutiliza o mesmo `session_key`.

## Requisitos

- Go 1.22+
- Docker (opcional)
- Docker Swarm + Traefik (opcional para producao)
- Token do gateway GoClaw

## Variaveis de ambiente

- `GOCLAW_TOKEN`: token do GoClaw (obrigatoria, alternativa a `GOCLAW_TOKEN_FILE`)
- `GOCLAW_TOKEN_FILE`: caminho de arquivo com token (ideal para Docker secret)
- `GOCLAW_WS_URL`: default `wss://ws.geoclaw.pullse.ia.br/ws`
- `PORT`: default `8080`

## Endpoints

- `GET /health`
- `POST /v1/chat` (sincrono)
- `POST /v1/jobs` (assincrono)
- `GET /v1/jobs/{id}` (status/resultado do job)

## Exemplo de request

```json
{
  "user_id": "alanweb7",
  "agent_id": "luna-clara",
  "session_key": "agent:luna-clara:ws:direct:1234",
  "message": "Meu nome e Alan. Qual e meu nome?",
  "stream": true,
  "locale": "pt-BR"
}
```

## Rodar local (Go)

```bash
go mod tidy
GOCLAW_TOKEN='SEU_TOKEN' go run .
```

Teste:

```bash
curl --location 'http://localhost:8080/v1/chat' \
  --header 'Content-Type: application/json' \
  --data '{
    "user_id":"alanweb7",
    "agent_id":"luna-clara",
    "session_key":"agent:luna-clara:ws:direct:1234",
    "message":"Qual nome eu te pedi para lembrar?",
    "stream":true
  }'
```

### Jobs assincronos com callback opcional

Criar job:

```bash
curl --location 'http://localhost:8080/v1/jobs' \
  --header 'Content-Type: application/json' \
  --data '{
    "user_id":"alanweb7",
    "agent_id":"luna-clara",
    "session_key":"agent:luna-clara:ws:direct:1234",
    "message":"Qual nome eu te pedi para lembrar?",
    "stream":true,
    "callback_url":"https://webhook.site/seu-id",
    "callback_headers":{"X-Source":"goclaw-api"}
  }'
```

Resposta esperada:

```json
{"id":"job_xxx","status":"queued"}
```

Consultar status:

```bash
curl --location 'http://localhost:8080/v1/jobs/job_xxx'
```

Quando finalizar, o callback (se informado) recebe `POST` com o JSON completo do job.

## Rodar com Docker (local)

```bash
docker build -t goclaw-api:latest .
docker run --rm -p 8080:8080 \
  -e GOCLAW_TOKEN='SEU_TOKEN' \
  -e GOCLAW_WS_URL='wss://ws.geoclaw.pullse.ia.br/ws' \
  goclaw-api:latest
```

## Deploy no Docker Swarm com Traefik (subdominio)

Usa o arquivo `stack.yml` e publica em algo como `https://api-goclaw.seu-dominio.com`.

1. Criar secret (uma vez):

```bash
printf '%s' 'SEU_TOKEN_AQUI' | docker secret create goclaw_token -
```

2. Build e push da imagem:

```bash
docker build -t REGISTRY_HOST/goclaw-api:1.0.0 .
docker push REGISTRY_HOST/goclaw-api:1.0.0
```

3. Deploy:

```bash
export GOCLAW_API_IMAGE='REGISTRY_HOST/goclaw-api:1.0.0'
export GOCLAW_API_HOST='api-goclaw.seu-dominio.com'
export TRAEFIK_CERTRESOLVER='letsencrypt'

docker stack deploy -c stack.yml goclaw
docker service ls
docker service logs -f goclaw_goclaw-api
```

4. Teste:

```bash
curl --location 'https://api-goclaw.seu-dominio.com/v1/chat' \
  --header 'Content-Type: application/json' \
  --data '{
    "user_id":"alanweb7",
    "agent_id":"luna-clara",
    "session_key":"agent:luna-clara:ws:direct:1234",
    "message":"Qual nome eu te pedi para lembrar?",
    "stream":true
  }'
```

## Deploy no Docker Swarm com PathPrefix (sem subdominio)

Usa o arquivo `stack.pathprefix.yml` e publica em algo como:
`https://app.seu-dominio.com/goclaw-api`.

```bash
export GOCLAW_API_IMAGE='REGISTRY_HOST/goclaw-api:1.0.0'
export GOCLAW_API_HOST='app.seu-dominio.com'
export GOCLAW_API_PREFIX='/goclaw-api'
export TRAEFIK_CERTRESOLVER='letsencrypt'

docker stack deploy -c stack.pathprefix.yml goclaw
docker service ls
docker service logs -f goclaw_goclaw-api
```

Teste:

```bash
curl --location 'https://app.seu-dominio.com/goclaw-api/v1/chat' \
  --header 'Content-Type: application/json' \
  --data '{
    "user_id":"alanweb7",
    "agent_id":"luna-clara",
    "session_key":"agent:luna-clara:ws:direct:1234",
    "message":"Qual nome eu te pedi para lembrar?",
    "stream":true
  }'
```

## Estrutura do projeto

- `main.go`: API HTTP e cliente WS para GoClaw.
- `Dockerfile`: imagem de producao.
- `stack.yml`: stack Swarm com Traefik por subdominio.
- `stack.pathprefix.yml`: stack Swarm com Traefik por prefixo.

## Observacoes

- Reutilize o mesmo `session_key` para manter memoria da conversa.
- Em producao, prefira `GOCLAW_TOKEN_FILE` com Docker secret.
