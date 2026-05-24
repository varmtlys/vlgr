# VLGR на VPS: деплой с Caddy и Cloudflare

Схема продакшен-развёртывания без конфликтов с другими проектами на том же VPS.

---

## Архитектура

```
Интернет
    │
    ▼
Cloudflare DNS (только DNS, серый облак)
    │  A  tunnel.domain.com       → VPS IP
    │  A  *.tunnel.domain.com     → VPS IP
    ▼
Caddy (VPS, порты 443 и 80)
    │  tunnel.domain.com          → https + прокси на :4443  (WebSocket)
    │  *.tunnel.domain.com        → https + прокси на :8080  (внешний HTTP)
    ▼
VLGR Server (VPS, только localhost)
    │  127.0.0.1:4443  — WebSocket для клиентов
    │  127.0.0.1:8080  — HTTP для внешних запросов
```

**VLGR не слушает внешние интерфейсы** — только `127.0.0.1`. Весь TLS и маршрутизация на Caddy. Конфликт с другими проектами исключён: VLGR живёт под subdomain'ом `tunnel.domain.com`, а остальные проекты — под своими.

---

## Шаг 1: Cloudflare DNS

Зайди в панель Cloudflare → DNS → Records. Добавь две записи:

| Тип | Имя | Значение | Proxy |
|---|---|---|---|
| A | `tunnel` | `<IP твоего VPS>` | **Выкл** (серый облак) |
| A | `*.tunnel` | `<IP твоего VPS>` | **Выкл** (серый облак) |

**Важно:** проксирование (оранжевый облак) выключено. Cloudflare на бесплатном тарифе не проксирует wildcard-поддомены. Caddy сам выпустит сертификаты через Let's Encrypt.

---

## Шаг 2: Caddy — установка плагина Cloudflare DNS

Для wildcard-сертификата (`*.tunnel.domain.com`) Let's Encrypt требует DNS-01 проверку. Нужен плагин `caddy-dns/cloudflare`.

```bash
# Если Caddy установлен через официальный пакет:
sudo caddy add-package github.com/caddy-dns/cloudflare

# Если собран вручную через xcaddy:
xcaddy build --with github.com/caddy-dns/cloudflare
```

**API-токен Cloudflare:** в панели Cloudflare → Profile → API Tokens → Create Token:
- Шаблон: «Edit zone DNS»
- Zone: выбери `domain.com`
- Сохрани токен (покажется один раз)

---

## Шаг 3: Caddy — конфигурация

Создай файл `Caddyfile` или дополни существующий. Пример для `/etc/caddy/Caddyfile`:

```caddyfile
# ============================
# VLGR Tunnel
# ============================

# WebSocket endpoint для VLGR-клиентов
tunnel.domain.com {
    reverse_proxy 127.0.0.1:4443
}

# Публичные HTTP-запросы через поддомены туннелей
*.tunnel.domain.com {
    tls {
        dns cloudflare {env.CF_API_TOKEN}
    }
    reverse_proxy 127.0.0.1:8080
}

# ============================
# Твои остальные проекты
# ============================
# project1.domain.com { ... }
# project2.domain.com { ... }
```

**Переменная окружения:** Caddy нужен API-токен Cloudflare. Добавь в systemd-юнит Caddy (обычно `/etc/systemd/system/caddy.service`):

```
[Service]
Environment=CF_API_TOKEN=твой-токен-сюда
```

Или через `/etc/caddy/.env` (если настроен).

После правок:

```bash
sudo systemctl daemon-reload
sudo systemctl reload caddy
```

Проверка:
```bash
curl https://tunnel.domain.com/_tunnel
# Должен вернуть "Bad Request" (не WebSocket) — значит Caddy проксирует
```

---

## Шаг 4: VLGR Server — сборка и запуск

### Сборка на VPS

```bash
cd vlgr
go mod tidy
./scripts/build.sh --linux-only
# Бинарники в build/linux/vlgr-server и vlgr-client
```

### Запуск вручную (для теста)

```bash
./build/linux/vlgr-server \
    -addr 127.0.0.1:4443 \
    -http 127.0.0.1:8080 \
    -domain tunnel.domain.com
```

Сервер слушает **только localhost**. Прямого доступа из интернета к портам 4443/8080 нет — только через Caddy.

### Systemd-юнит (автозапуск)

Создай `/etc/systemd/system/vlgr-server.service`:

```ini
[Unit]
Description=VLGR Tunnel Server
After=network.target caddy.service

[Service]
Type=simple
User=nobody
Group=nogroup
WorkingDirectory=/opt/vlgr
ExecStart=/opt/vlgr/vlgr-server -addr 127.0.0.1:4443 -http 127.0.0.1:8080 -domain tunnel.domain.com
Restart=always
RestartSec=5

# Безопасность
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/opt/vlgr

[Install]
WantedBy=multi-user.target
```

```bash
# Скопировать бинарник
sudo mkdir -p /opt/vlgr
sudo cp build/linux/vlgr-server /opt/vlgr/

# Активировать
sudo systemctl daemon-reload
sudo systemctl enable vlgr-server
sudo systemctl start vlgr-server
sudo systemctl status vlgr-server
```

---

## Шаг 5: VLGR Client — подключение с твоей машины

```bash
# С локальной машины (не VPS)
./vlgr-client -server tunnel.domain.com:443 -local 3000 -token my-secret
```

Клиент подключается по WSS (через Caddy → TLS → VLGR). Порт 443, потому что Caddy проксирует `tunnel.domain.com:443` → `localhost:4443`.

**Важно:** сейчас клиент подключается по `ws://` (без TLS). Для продакшена нужно заменить `ws://` на `wss://` в коде клиента — либо добавить флаг `-tls`. Но поскольку перед VLGR стоит Caddy с TLS, фактически соединение шифруется. Клиенту нужно указать `wss://` вместо `ws://`.

В текущем коде клиента URL собирается так (в `internal/client/tunnel.go`):

```go
url := "ws://" + t.serverAddr + "/_tunnel"
```

Надо заменить на:

```go
url := "wss://" + t.serverAddr + "/_tunnel"
```

Или добавить флаг `-tls` в клиент. Я поправлю код.

---

## Проверка всей цепочки

```bash
# 1. На VPS: статус VLGR
sudo systemctl status vlgr-server

# 2. На VPS: Caddy сертификаты
sudo ls /var/lib/caddy/.local/share/caddy/certificates/

# 3. На локальной машине: запуск клиента
./vlgr-client -server tunnel.domain.com:443 -local 3000
# Вывод: Tunnel: a3f8b2c1.tunnel.domain.com -> localhost:3000

# 4. Из любого места: тестовый запрос
curl https://a3f8b2c1.tunnel.domain.com/

# 5. Проверить, что другие проекты не сломаны
curl https://project1.domain.com/
```

---

## Почему это не конфликтует с другими проектами

1. **VLGR сервер слушает только 127.0.0.1** — порты 4443 и 8080 недоступны снаружи. Никакой другой сервис не может «случайно» получить запрос, предназначенный VLGR.

2. **Caddy маршрутизирует по домену**, а не по порту:
   - `tunnel.domain.com` → VLGR WebSocket
   - `*.tunnel.domain.com` → VLGR HTTP
   - `project1.domain.com` → проект 1
   - `project2.domain.com` → проект 2
   
   Всё на одних и тех же портах 80/443. Caddy различает запросы по заголовку `Host`.

3. **Отдельный subdomain-префикс `tunnel.`** — DNS-записи `*.tunnel.domain.com` не пересекаются с DNS-записями других проектов.

4. **Wildcard-сертификат на `*.tunnel.domain.com`** отделён от сертификатов других поддоменов — Caddy выпускает сертификат только на нужный wildcard, не трогая остальные.
