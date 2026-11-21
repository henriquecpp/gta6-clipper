# gta6-clipper

Clipador automático de lives da **Twitch** com **aprovação humana via Telegram**.

1. Monitora as lives de uma allowlist de streamers.
2. Detecta "melhores momentos" por **voz** (energia da fala, com VAD) e **chat** (taxa de mensagens e
   emotes), em tempo real, e confirma com a **chuva de clipes** dos espectadores (sinal tardio).
3. Guarda a janela de vídeo do momento, gera uma **prévia** 480p e envia ao seu Telegram com os botões
   **✅ Aprovar** e **🗑️ Descartar**.
4. Só depois da aprovação: transcrição local na GPU (whisper.cpp) → **Claude** escolhe o corte, escreve a
   manchete, a legenda em PT-BR (traduzida se preciso) e o texto do post → render vertical 9:16 com legenda
   queimada (ffmpeg + NVENC) → vídeo e texto entregues no Telegram.
5. **Nunca publica nada automaticamente.** A publicação é manual, sua.

**Regra de custo:** monitoramento, detecção, transcrição e prévia custam zero token de IA. O Claude só é
chamado para clipes aprovados (meta: < US$ 0,05 por vídeo; com Haiku 5.5 fica em torno de US$ 0,001).

Andamento e pendências: [docs/PROGRESS.md](docs/PROGRESS.md) · Decisões: [docs/decisions/](docs/decisions/) ·
Instruções do projeto: [CLAUDE.md](CLAUDE.md)

---

## Sumário
1. [Como funciona](#como-funciona)
2. [Requisitos](#requisitos)
3. [Setup no Windows do zero](#setup-no-windows-do-zero)
4. [Configuração](#configuração)
5. [Obtendo os tokens](#obtendo-os-tokens)
6. [Primeira execução](#primeira-execução)
7. [Operação](#operação)
8. [Ajuste da detecção](#ajuste-da-detecção)
9. [Observabilidade](#observabilidade)
10. [Troubleshooting](#troubleshooting)
11. [Riscos legais](#riscos-legais)
12. [Desenvolvimento](#desenvolvimento)

---

## Como funciona

```
Twitch ──► MonitorLives (polling Get Streams a cada 45 s)
              │ live começou
              ▼
         Supervisor por live ──► Streamlink → ffmpeg ──► segmentos de 2 s (buffer circular, tmpfs)
              │                                   └────► PCM 16 kHz → VAD → energia da voz
              ├── EventSub WebSocket (chat) ─────────────► taxa de chat + emotes
              └── Get Clips (a cada 30 s) ───────────────► chuva de clipes (sinal tardio)
                              │
                              ▼
                  DetectMoment (baseline EWMA, z-scores, fusão, histerese, cooldown)
                              │ momento aceito
                              ▼
     retém a janela (sem re-encode) → clipe "detected" → prévia 480p → Telegram [Aprovar][Descartar]
                              │ você aprova
                              ▼
     whisper (GPU) → Claude (corte, manchete, legenda, post) → render 9:16 + ASS → Telegram
```

- **Arquitetura hexagonal** (ports & adapters): `domain` (regras puras) ← `app` (casos de uso e portas) ←
  `adapters` (Twitch, Telegram, Claude, whisper, ffmpeg, SQLite) ← `cmd/clipper` (composição). A regra de
  dependência é imposta pelo lint (depguard).
- **Estado do clipe:** `detected → preview_sent → approved → editing → rendered → delivered`, com
  `discarded`, `expired` e `failed` (retentativa manual com `/retry`). Toda transição é feita em transação,
  então cliques duplicados são inofensivos.
- **Persistência:** SQLite (pure Go) com sqlc + goose; migrations aplicadas na inicialização.

## Requisitos

| Item | Testado com |
|---|---|
| Windows 11 com WSL2 | Windows 11 Pro |
| GPU NVIDIA com NVENC | RTX 5060 Ti 16 GB (Blackwell, sm_120 → exige CUDA 12.8+) |
| Driver NVIDIA recente | 621.24 |
| Docker Desktop (backend WSL2, GPU) | 29.9.3 |
| RAM | 16 GB (8 GB+ para o WSL2; o buffer usa tmpfs de até 6 GB) |
| Disco | ~15 GB (imagens + modelo do whisper de 1,6 GB + vídeos) |

ffmpeg, Streamlink e whisper.cpp **não** precisam estar no Windows: rodam nos containers.

Para desenvolver: Go 1.27.4, go-task, sqlc, golangci-lint (ver [Desenvolvimento](#desenvolvimento)).

## Setup no Windows do zero

### 1. Driver NVIDIA
Instale o driver Game Ready/Studio mais recente (Blackwell exige um driver com suporte a CUDA 12.8+).
Confira no PowerShell:
```powershell
nvidia-smi
```
Não instale CUDA dentro do WSL: o driver do Windows já expõe a GPU para o WSL2.

### 2. WSL2
```powershell
wsl --install        # se ainda não tiver
wsl --update
wsl --version        # WSL 2.x
```
Ajuste a memória do WSL2 (o Docker Desktop roda nele). Crie/edite `%UserProfile%\.wslconfig`:
```ini
[wsl2]
memory=12GB      # o buffer de captura usa tmpfs (RAM): 6 GB por padrão
processors=12
swap=8GB
```
Depois: `wsl --shutdown` e reabra o Docker Desktop.

### 3. Docker Desktop
- Instale o Docker Desktop, **Settings → General → Use the WSL 2 based engine**.
- Teste a GPU num container:
```powershell
docker run --rm --gpus all nvidia/cuda:12.8.1-base-ubuntu24.04 nvidia-smi
```

### 4. Projeto
```powershell
git clone <este repositório> gta6-clipper
cd gta6-clipper
Copy-Item .env.example .env
Copy-Item config.example.yaml config.yaml
```

### 5. (Opcional, para desenvolver) Ferramentas
```powershell
# Go 1.27.4: instalador oficial em https://go.dev/dl/ (ou o zip em %USERPROFILE%\sdk\go1.27.4)
go install github.com/go-task/task/v3/cmd/task@latest
go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
# Garanta que %USERPROFILE%\go\bin (e o bin do Go) estejam no PATH.
```
Sem o go-task, use os scripts PowerShell de [`scripts/`](scripts/) (equivalentes a `task up`, `task down`,
`task logs`, `task gpu:check`, `task db:backup`).

## Configuração

### `.env` (segredos — nunca versionado)
| Variável | Para quê |
|---|---|
| `TWITCH_CLIENT_ID`, `TWITCH_CLIENT_SECRET` | App da Twitch (detecção de lives, clipes, chat) |
| `TELEGRAM_BOT_TOKEN` | Bot que envia prévias e recebe aprovações |
| `TELEGRAM_ALLOWED_CHAT_IDS` | Chats que recebem prévias e podem comandar o bot (separados por vírgula) |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_MODEL` | Editor (Claude); ex.: `claude-haiku-5-5` |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` |

Sem alguma credencial o app **sobe em modo degradado** e avisa no log o que ficou desligado
([ADR 0016](docs/decisions/0016-modo-degradado.md)). Valores malformados impedem a inicialização com uma lista
clara dos erros.

### `config.yaml` (comportamento)
Comentado em [`config.example.yaml`](config.example.yaml). O essencial:

```yaml
streamers:
  - login: nome_do_streamer   # login da Twitch (minúsculo)
    language: en               # idioma da fala: escolhe os emotes de risada (en, pt...)
    allows_clips: true         # só canais com true são monitorados
    layout: blur               # blur | stack | crop
    # stack: { facecam: [0.70, 0.00, 0.30, 0.30], gameplay: [0, 0, 1, 1] }  # [x, y, w, h] de 0 a 1
    offsets: { capture_latency: 4s, chat: 8s, clip: 5s }
```

- **Layouts:** `blur` — vídeo 16:9 no meio sobre ele mesmo ampliado e desfocado; `stack` — câmera em cima
  (35%) e jogo embaixo, com as regiões em coordenadas normalizadas do quadro original; `crop` — recorte central.
- **`approval.ttl`** (6 h): prévias não decididas expiram e a mídia é apagada.
- **`render`**: resolução, fps (30/60), duração mínima/máxima do corte, `max_file_bytes` (≤ 50 MiB, limite
  do Telegram), fonte e caracteres por linha da legenda.
- **`editor`**: `max_tokens`, `timeout`, `max_cost_per_clip_usd` (alerta), `tool_choice` e **`pricing`**
  (US$ por milhão de tokens do modelo escolhido — o código não tem preços embutidos).
- **`concurrency`**: lives simultâneas, workers de ffmpeg, transcrição e render (GPU).

## Obtendo os tokens

### Twitch
1. Acesse <https://dev.twitch.tv/console/apps> → **Register Your Application** (a conta precisa de e-mail
   verificado e **2FA ativado**).
   - Name: único entre todos os apps da Twitch (ex.: `clipper-<seu-login>`); OAuth Redirect URLs:
     `http://localhost` (o Device Code Flow não usa, mas o campo é obrigatório); Category:
     *Application Integration*; Client Type: **Confidential** (o tipo *Public* não pode usar o fluxo client
     credentials, de que o app token precisa).
2. Copie **Client ID** e clique em **New Secret** → `.env`. Gerar um novo secret invalida o anterior.
3. Autorize o usuário que vai "ler o chat" (Device Code Flow, escopo `user:read:chat`):
   ```powershell
   task auth:twitch
   # ou: docker compose run --rm --no-deps app twitch-auth
   ```
   Abra a URL mostrada, digite o código e autorize. O token fica no volume `clipper-data`
   (`/data/twitch_user_token.json`, permissão 0600), é renovado automaticamente (o refresh token é
   rotativo) e validado a cada hora. Se a renovação falhar (senha trocada, app desconectado), o log pede
   `task auth:twitch` de novo e o chat fica desligado até lá — voz e clipes continuam funcionando.
4. O token é lido na inicialização: depois de autorizar, `docker compose restart app`. O log
   `live monitoring enabled` deve mostrar `"chat":true`.

O app token (detecção de lives e clipes) é obtido sozinho a partir do Client ID/Secret.

### Telegram
1. No Telegram, fale com **@BotFather** → `/newbot` → copie o token → `TELEGRAM_BOT_TOKEN`.
2. Mande qualquer mensagem para o seu bot e abra no navegador
   `https://api.telegram.org/bot<TOKEN>/getUpdates`; o número em `message.chat.id` é o seu chat id →
   `TELEGRAM_ALLOWED_CHAT_IDS` (pode listar vários, inclusive grupos, que têm id negativo).
3. O bot usa *long polling*: não precisa abrir porta. Se o bot já tiver um webhook configurado por outro
   programa, remova com `https://api.telegram.org/bot<TOKEN>/deleteWebhook`.

Uploads de bots são limitados a 50 MB (o render respeita `max_file_bytes`). Um servidor local da Bot API
(<https://github.com/tdlib/telegram-bot-api>) permite até 2000 MB, mas não é necessário.

### Anthropic (Claude)
1. Crie uma chave em <https://platform.claude.com> → `ANTHROPIC_API_KEY`.
2. Escolha o modelo em `ANTHROPIC_MODEL`. Sugestão: **`claude-haiku-5-5`** (rápido, barato e aceita tool use
   forçado).
3. Preencha `editor.pricing` com os preços atuais do modelo (US$/MTok). Valores da página de preços em
   2026-11-18:

   | Modelo | input | output | cache_write | cache_read | `tool_choice` |
   |---|---|---|---|---|---|
   | `claude-haiku-5-5` | 0.10 | 0.50 | 0.125 | 0.01 | `tool` |
   | `claude-sonnet-5-5` | 2.00 | 10.00 | 2.50 | 0.10 | **`auto`** |
   | `claude-opus-5-5` | 4.00 | 20.00 | 5.00 | 0.20 | **`auto`** |

   Opus 5.5, Sonnet 5.5 e Fable 5.x rejeitam tool use forçado; para eles use `editor.tool_choice: auto`
   (o app usa a ferramenta em modo `strict`). Ver [ADR 0005](docs/decisions/0005-claude-sdk-oficial.md) e
   [ADR 0015](docs/decisions/0015-editor-claude.md).

## Primeira execução

```powershell
task build            # imagens do app e do whisper (o whisper compila CUDA: ~10–20 min na 1ª vez)
task whisper:model    # baixa o ggml-large-v3-turbo (1,6 GB) para o volume whisper-models
task up               # ou .\scripts\up.ps1
task gpu:check        # ou .\scripts\gpu-check.ps1 — CUDA sm_120, NVENC e timestamps por palavra
task logs             # ou .\scripts\logs.ps1 -Service app
```

O `gpu:check` deve terminar com `OK: whisper.cpp is using CUDA on an sm_120 device` e
`OK: all GPU checks passed`.

Quer ver a captura funcionando antes de configurar credenciais? Escolha qualquer canal público ao vivo:
```powershell
docker compose run --rm --no-deps --entrypoint clipper app probe -duration 60s <login>
```
Ele captura por 60 s, mostra frames de áudio/fala, retém 20 s e estima a latência HLS.

## Operação

### No Telegram
- Cada prévia chega como vídeo com: streamer e jogo, score (voz · chat · clipes), posição do pico na live e
  em quanto tempo expira, com **✅ Aprovar** e **🗑️ Descartar**.
- A mensagem é editada conforme o estado: "✅ Aprovado — gerando…", "✅ Entregue", "🗑️ Descartado",
  "⌛ Expirado", "⚠️ Falhou: …" (com botão **🔁 Tentar de novo**).
- O vídeo final chega com a manchete na legenda; o **texto do post** vem na mensagem seguinte, pronto para
  copiar. O arquivo também fica em `.\output\<data>_<streamer>_<id>.mp4`.
- Comandos: `/status` (lives, fila, último erro, custo do dia), `/pending` (prévias aguardando),
  `/retry <clip_id>` (reprocessa um clipe que falhou; o plano do Claude já pago é reaproveitado).
- Mensagens e botões de chats fora da allowlist são ignorados.

### Rotina
| Ação | Comando |
|---|---|
| Subir / derrubar | `task up` / `task down` (`scripts\up.ps1`, `scripts\down.ps1`) |
| Subir com Prometheus + Grafana | `task up:obs` (`scripts\up.ps1 -Obs`) |
| Logs | `task logs` (`scripts\logs.ps1 -Service app`) |
| Backup do banco | `task db:backup` (`scripts\backup.ps1`) → `.\backups\` |
| Reautorizar a Twitch | `task auth:twitch` |
| Testar transcrição + render sem Claude | `docker compose run --rm --no-deps app dryrun` (lista clipes com mídia retida) e `... app dryrun <clip_id>` |

- **`dryrun`:** transcreve a janela retida de um clipe real, monta um corte sem IA (≈30 s em volta do pico,
  legenda com a fala original, sem tradução) e renderiza em `.\output`. Não muda o estado do clipe, não
  envia nada e não chama o Claude; serve para validar whisper e render com mídia real.

- **Reinício:** ao subir, o app retoma o que ficou pela metade (reenvia prévias de clipes `detected`,
  reprocessa `approved`/`editing`/`rendered`) e apaga mídia órfã.
- **Desligamento** (`task down`/SIGTERM): a ingestão para, jobs em andamento têm `http.shutdown_timeout`
  (45 s) para terminar; o resto é retomado no próximo início.
- **Disco:** o buffer (tmpfs) é limitado por `capture.buffer_duration` e `capture.buffer_max_bytes`; janelas
  retidas e prévias ficam no volume `clipper-pending` e são apagadas ao descartar, expirar ou entregar. Os
  vídeos finais em `.\output` são seus: apague quando quiser.

## Ajuste da detecção

Parâmetros em `detection` ([ADR 0017](docs/decisions/0017-modelo-de-deteccao.md)):
- `threshold_on`/`threshold_off` (histerese), `weights` (voz, chat, clipes), `cooldown`, `warmup` (5 min sem
  disparos no início de cada live), `min_signal_z` (voz ou chat precisam passar desse z).
- `pre_roll`/`post_roll`: janela retida em volta do pico (60 s antes, 30 s depois).
- `chat.emotes` por idioma (peso da mensagem = maior peso dos termos que ela contém).
- **Offsets por streamer** (`capture_latency`, `chat`, `clip`): os padrões (4 s, 8 s, 5 s) são estimativas.
  Para calibrar um streamer: em alguns momentos com grito e explosão de chat, compare no log
  (`moment detected`, campos `voice`/`chat`) ou no Grafana (painel de z-scores) o instante do pico de voz com o
  do chat; ajuste `chat` até os dois picos coincidirem. `clipper probe` mede a latência HLS (2–3 s até o
  segmento mais novo) — `capture_latency` soma a isso o atraso de codificação do streamer.
- A tabela `decisions` do banco guarda score e sinais de cada aprovação/descarte: é o dataset para calibrar
  pesos e limiares.

## Observabilidade

`task up:obs` sobe Prometheus (<http://localhost:9090>) e Grafana (<http://localhost:3000>, acesso anônimo de
leitura), com o dashboard **gta6-clipper** provisionado: lives, z-scores, momentos por hora, funil de clipes,
latência pico → prévia (meta < 30 s), duração por etapa, custo diário e tokens do Claude, erros por porta
externa, GPU e NVENC. Métricas brutas em <http://localhost:8080/metrics> (prefixo `clipper_`).

Endpoints internos (só em `localhost`): `/healthz`, `/readyz`, `/metrics`.

## Troubleshooting

| Sintoma | Causa provável / solução |
|---|---|
| `no kernel image is available` no whisper | Build sem `CMAKE_CUDA_ARCHITECTURES=120` ou CUDA < 12.8. Use o `deploy/whisper.Dockerfile` (CUDA 12.8.1, sm_120) e `task build`. |
| `h264_nvenc` ausente / `Cannot load libnvidia-encode.so.1` | O container precisa da capability `video` (já no `docker-compose.yml`); atualize o driver. O app cai sozinho para `libx264` (`clipper_nvenc_available 0`). |
| `docker run --gpus all ... nvidia-smi` falha | Docker Desktop sem backend WSL2 ou driver antigo; `wsl --update` e reinicie o Docker Desktop. |
| whisper responde 503 | Ainda carregando o modelo (~10 s); o app repete. Se persistir: `task whisper:model` (modelo ausente). |
| Lives não aparecem | `TWITCH_CLIENT_ID/SECRET` ausentes (log de modo degradado) ou login errado na allowlist; `allows_clips: false` ignora o canal. |
| "twitch user token unavailable/invalid" | Rode `task auth:twitch`. Sem ele só o chat fica desligado. |
| Prévias não chegam | Token/chat id do Telegram; mande `/status` ao bot. Webhook ativo impede o long polling (`deleteWebhook`). |
| Clipe `failed` com "anthropic is not configured" | Preencha `ANTHROPIC_API_KEY`/`ANTHROPIC_MODEL` e use `/retry`. |
| Erro 400 do Claude sobre `tool_choice` | Modelo que não aceita tool use forçado: `editor.tool_choice: auto`. |
| Captura reinicia em loop | Live offline/raid ou intervalo comercial; veja `capture stopped; restarting` nos logs. O polling de lives encerra o supervisor quando a live acaba. |
| Memória do WSL2 esgotada | Aumente `memory` no `.wslconfig` ou reduza `capture.buffer_max_bytes` e o `size` do tmpfs em `docker-compose.yml`. |
| Scripts falham no container com `\r` | Fins de linha: mantenha o `.gitattributes` (`eol=lf`) e reclone se necessário. |
| Aviso `volume "whisper-models" already exists but was not created by Docker Compose` | Inofensivo: o volume foi criado fora do Compose (ex.: download manual do modelo). |
| SQLite "database is locked" | Banco em bind mount do Windows. Use o volume `clipper-data` (padrão). |

## Riscos legais

**Leia antes de usar.** Este projeto captura e reedita transmissões de terceiros:
- **Termos da Twitch:** capturar streams (Streamlink) e redistribuir trechos pode violar os Termos de
  Serviço e as Diretrizes da Comunidade da Twitch e o Developer Agreement da API. Clipes oficiais da Twitch
  (criados na plataforma) têm regras próprias.
- **Direitos autorais:** o conteúdo é do **streamer**; o jogo (ex.: GTA VI) é propriedade da **publicadora**;
  **músicas licenciadas** tocadas na live podem gerar strikes/DMCA nas redes onde você publicar.
- O projeto só monitora canais da allowlist com `allows_clips: true`, sempre credita o streamer no texto do
  post e **nunca publica nada automaticamente** — mas a decisão de publicar, e a responsabilidade por obter
  autorização, são **suas**. Peça permissão aos streamers.
- A captura fica atrás da porta `MediaCapture`; é possível trocar a fonte por clipes oficiais se necessário.

## Desenvolvimento

| Comando | O que faz |
|---|---|
| `task check` | sqlc sem diff + `go vet` + golangci-lint (depguard) + testes com `-race` (no Windows, no container) |
| `task test` | testes unitários no host |
| `task test:integration` | integração no container (captura, render, pipeline ponta a ponta) |
| `task test:gpu` | whisper real (CUDA) e NVENC real |
| `task cover` | cobertura de `domain` e `app` (alvo ≥ 80%) |
| `task generate` | `sqlc generate` (nunca edite `sqlcgen/` à mão) |

- Camadas de teste: domínio table-driven + fuzz; casos de uso com fakes escritos à mão (`internal/app/fakes`);
  suíte de contrato do repositório (`internal/app/contracttest`) contra SQLite e fake; adaptadores HTTP com
  `httptest` (Twitch, Claude, whisper) e Bot API fake (`internal/platform/telegramtest`); argumentos do ffmpeg
  e legendas com golden files (`testdata/golden`, regenere com `-update`).
- `scripts/make-testdata.ps1` regenera as amostras de fala (TTS do Windows, com o instante de cada palavra
  como ground truth) e o vídeo de exemplo.
- Regras de arquitetura, convenções e roadmap: [CLAUDE.md](CLAUDE.md). Decisões: [docs/decisions/](docs/decisions/).
