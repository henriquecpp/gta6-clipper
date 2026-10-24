# gta6-clipper

Clipador automático de lives da Twitch com aprovação humana via Telegram. Detecta melhores momentos por
voz e chat (confirmados pela chuva de clipes dos espectadores), envia uma prévia ao Telegram e, só após a
aprovação, transcreve localmente (whisper.cpp na GPU), pede ao Claude o corte/manchete/legenda e renderiza
um vídeo vertical 9:16 com legenda queimada. **Nunca publica nada automaticamente.**

> Documentação completa (setup no Windows, tokens, operação, troubleshooting e riscos legais) é escrita na
> Fase 6. Andamento em [docs/PROGRESS.md](docs/PROGRESS.md); decisões em [docs/decisions/](docs/decisions/).

## Início rápido (desenvolvimento)
```powershell
# Go 1.27.4, go-task, sqlc, golangci-lint e Docker Desktop (WSL2 + GPU) instalados
Copy-Item .env.example .env
Copy-Item config.example.yaml config.yaml
task build
task whisper:model   # baixa o large-v3-turbo para o volume whisper-models
task up
task gpu:check
task check
```
