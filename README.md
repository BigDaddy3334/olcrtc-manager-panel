# olcrtc-manager-panel

Веб-панель и менеджер процессов для запуска нескольких экземпляров сервера `olcrtc`.

Версия 2 включает:

- админ-панель по адресу `/admin`;
- авторизацию администратора через сгенерированный или заданный пароль;
- создание, редактирование и удаление клиентов, ротацию комнат/ключей, перезапуск, логи, QR и экспорт подписок;
- подписки для каждого клиента по адресу `/<client-id>/`;
- метаданные квот трафика в подписках;
- автоматический учет входящего трафика;
- отображение текущего числа подключенных peers из логов `olcrtc`;
- блокировку при превышении лимита трафика и по сроку действия;
- ограничения скорости через отдельный для клиента `network namespace` + `veth`;
- по одному изолированному процессу `olcrtc` на каждую локацию клиента.

## Требования

Менеджер должен запускаться в Linux с правами root, потому что v2 создает сетевые пространства имен, veth-интерфейсы, маршруты, правила iptables и ограничения `tc` qdisc.

Необходимые инструменты на сервере:

```sh
ip
iptables
tc
systemctl
```

Файлы времени выполнения, ожидаемые стандартным systemd unit:

- `/usr/local/bin/olcrtc-manager`
- `/usr/local/bin/olcrtc`, собранный из ветки `master` репозитория `openlibrecommunity/olcrtc`
- `/etc/olcrtc-manager/config.json`
- необязательный `/etc/olcrtc-manager/panel.env`

Установщик создает `panel.env` сам и выводит сгенерированные логин/пароль. При ручной установке можно не создавать `panel.env`; тогда при первом открытии панель попросит создать пароль администратора.

## Сборка

Сначала соберите фронтенд-ассеты, затем Go-бинарник, чтобы панель была встроена в менеджер:

```sh
pnpm install
pnpm build
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o olcrtc-manager ./cmd/olcrtc-manager
```

Если вы изменяли только Go-код и `cmd/olcrtc-manager/web/dist` уже существует, достаточно выполнить `go build`.

## Установка

### Установка одной командой

Чистая установка на Debian/Ubuntu VPS:

```sh
curl -fsSL https://raw.githubusercontent.com/oleg54644/olcrtc-manager-panel-GM/main/scripts/install.sh | sudo bash
```

Установщик:

- устанавливает необходимые пакеты;
- устанавливает Go, если системный Go отсутствует или слишком старый;
- собирает и устанавливает `olcrtc` из ветки `master`;
- собирает и устанавливает `olcrtc-manager`;
- создает `/etc/olcrtc-manager/config.json` без начальных комнат, если файл еще не существует;
- сохраняет существующие `config` и `panel.env`;
- устанавливает и запускает `olcrtc-manager.service`.

По умолчанию сервис слушает `0.0.0.0:<random-port>`, включает HTTPS с самоподписанным сертификатом и сразу доступен по внешнему IP VPS. Чтобы оставить панель только на localhost для nginx/reverse proxy:

```sh
curl -fsSL https://raw.githubusercontent.com/oleg54644/olcrtc-manager-panel-GM/main/scripts/install.sh | sudo env PANEL_ADDR=127.0.0.1 bash
```

Установщик создает случайные логин и пароль администратора и выводит их в конце установки. Откройте выведенный установщиком URL и войдите с этими данными. Если панель опубликована напрямую, смените пароль после входа и ограничьте доступ файрволом при необходимости.
Браузер может предупредить о самоподписанном сертификате; это нормально для установки без домена и Let's Encrypt.
В чистой установке также нет комнат; после входа создайте клиентов и вставьте ID комнат вручную.

Опции установщика можно передавать через переменные окружения:

```sh
curl -fsSL https://raw.githubusercontent.com/oleg54644/olcrtc-manager-panel-GM/main/scripts/install.sh | \
  sudo env PANEL_PORT=9443 bash
```

### Ручная установка

Скопируйте бинарники и конфигурацию:

```sh
sudo install -m 0755 olcrtc-manager /usr/local/bin/olcrtc-manager
sudo install -m 0755 olcrtc /usr/local/bin/olcrtc
sudo install -d -m 0755 /etc/olcrtc-manager
sudo install -m 0600 config.json /etc/olcrtc-manager/config.json
```

Установите и запустите systemd-сервис:

```sh
sudo install -m 0644 packaging/systemd/olcrtc-manager.service /etc/systemd/system/olcrtc-manager.service
sudo systemctl daemon-reload
sudo systemctl enable --now olcrtc-manager
```

Проверьте статус:

```sh
sudo systemctl status olcrtc-manager
sudo journalctl -u olcrtc-manager -f
```

При установке через скрипт менеджер слушает `0.0.0.0:<config.port>` по HTTPS. При ручном запуске без `OLCRTC_MANAGER_ADDR` бинарь слушает `127.0.0.1:<config.port>` по HTTP, если не заданы `OLCRTC_MANAGER_TLS_CERT` и `OLCRTC_MANAGER_TLS_KEY`. В примерах по умолчанию используется порт `8888`.

## Первый запуск

Откройте панель:

```text
https://SERVER:8888/admin
```

При установке через скрипт логин и пароль уже записаны в `/etc/olcrtc-manager/panel.env` и выведены в консоль установщика. Если при ручной установке `panel.env` не существует или не содержит пароль, панель запускается в режиме первого запуска и предлагает задать пароль администратора.

После настройки менеджер записывает:

```sh
/etc/olcrtc-manager/panel.env
```

Пример содержимого:

```sh
OLCRTC_MANAGER_USER='admin'
OLCRTC_MANAGER_PASS='your-password'
```

После этого панель использует cookie-сессии для входа. Позже пароль можно изменить кнопкой `Пароль` в заголовке панели.

## Reverse Proxy

Для nginx/reverse proxy установите панель локально с `PANEL_ADDR=127.0.0.1` и проксируйте на неё:

```nginx
server {
    listen 9443 ssl http2;
    server_name example.com;

    ssl_certificate /path/fullchain.pem;
    ssl_certificate_key /path/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8888;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Затем откройте:

```text
https://example.com:9443/admin
```

## Конфигурация

Минимальная конфигурация:

```json
{
  "version": 1,
  "name": "OlcRTC VPS",
  "port": 8888,
  "refresh": "10m",
  "clients": [
    {
      "client-id": "default",
      "refresh": "5m",
      "quota": {
        "speed_mbps": 10,
        "traffic_gb": 100,
        "expires_at": "2026-12-31"
      },
      "locations": [
        {
          "name": "Current VPS",
          "endpoint": {
            "room_id": "https://meet.handyweb.org/room",
            "key": "e830d36f7be8cfb04a741fc1a5e2ddf8ff04f30985dc070616483f939ad5fafe"
          },
          "carrier": "jitsi",
          "transport": {
            "type": "datachannel"
          },
          "link": "direct",
          "data": "data",
          "dns": "1.1.1.1:53",
          "proxy": {
            "addr": "127.0.0.1",
            "port": 1080,
            "user": "optional-user",
            "pass": "optional-password"
          }
        }
      ]
    }
  ]
}
```

Поля квоты:

- `speed_mbps`: ограничение скорости для локации клиента. `0` или отсутствие поля означает отсутствие ограничения.
- `traffic_gb`: лимит трафика. `0` или отсутствие поля означает отсутствие ограничения.
- `used_bytes`: автоматически обновляется менеджером.
- `used_gb`: производное/устаревшее значение для отображения.
- `expires_at`: необязательная дата окончания срока действия в формате `YYYY-MM-DD`.

Поля подписки:

- `refresh`: интервал автообновления подписки в формате `5s`, `10m`, `6h` или `1d`.
- `refresh` на верхнем уровне применяется ко всем подпискам.
- `refresh` внутри клиента переопределяет глобальное значение только для подписки этого клиента.

Старый формат с `locations` на верхнем уровне по-прежнему принимается и нормализуется в `clients`.

Конфигурация менеджера остается JSON-файлом для данных панели, квот и подписок. Для каждой запущенной локации менеджер записывает временную runtime-конфигурацию `olcrtc` в YAML и запускает `olcrtc <config.yaml>`.

`carrier` сопоставляется с новым полем `auth.provider` в `olcrtc`. Поддерживаемые провайдеры: `jitsi`, `wbstream` и `telemost`. Для `jitsi` значение `endpoint.room_id` — это полный URL комнаты, например `https://meet.handyweb.org/room`. Для остальных провайдеров это ID комнаты провайдера. Значение `any` отклоняется.

`proxy` необязателен. Если он задан, менеджер пробрасывает его в runtime YAML как `socks.proxy_addr`, `socks.proxy_port`, `socks.proxy_user` и `socks.proxy_pass`. Это upstream SOCKS5-прокси, через который серверная сторона `olcrtc` будет открывать исходящие подключения; `user`/`pass` используются для RFC 1929-аутентификации.

## Сетевая изоляция и лимиты

Для каждой запущенной локации менеджер создает:

- сетевое пространство имен: `olc-*`;
- host veth: `olh*`;
- namespace veth: `oln*`;
- NAT-правило для исходящего трафика из namespace;
- DNS-файл в `/etc/netns/<namespace>/resolv.conf`;
- необязательное ограничение скорости `tc tbf` на обеих сторонах veth.

Полезные проверки:

```sh
ip netns list
ip -br link | grep olh
tc qdisc show dev olhXXXXXXXX
ip netns exec olc-XXXXXXXX tc qdisc show
iptables -t nat -S POSTROUTING | grep olcrtc-manager-netns
```

Учет трафика использует `tx_bytes` host veth, что соответствует трафику, отправленному с VPS в сторону namespace клиента. Когда настроенная квота трафика превышена, менеджер останавливает локацию этого клиента. Если увеличить `traffic_gb` выше `used_bytes`, reload/restart снова запустит ее.

## Подписки

Подписка клиента:

```text
http://127.0.0.1:8888/sub/<client-id>/
```

Если задан интервал обновления, подписка включает глобальное поле формата `sub.md`:

```text
#refresh: 5m
```

Если квота настроена, подписка включает ее метаданные:

```text
#quota-speed-mbps: 10
#quota-traffic-gb: 100
#quota-used-gb: 5
#quota-used-bytes: 5368709120
#quota-expires-at: 2026-12-31
#quota-status: active
```

Возможные статусы квоты:

- `active`
- `expired`
- `traffic_exceeded`

## Перезагрузка

Перезагрузите конфигурацию и примените изменения клиентов без перезапуска неизмененных процессов:

```sh
sudo systemctl reload olcrtc-manager
```

Или локально:

```sh
curl -X POST http://127.0.0.1:8888/-/reload
```

## API и авторизация панели

При установке через скрипт используйте логин и пароль, которые установщик вывел в конце. При ручной установке без `panel.env` настройку первого запуска нужно завершить из `/admin`.

После настройки:

- вход в UI использует cookie-сессию;
- Basic auth по-прежнему работает для скриптов и curl;
- пароль можно изменить из панели.

## Вспомогательные скрипты

В `scripts/` доступны небольшие вспомогательные скрипты для редактирования JSON-конфига:

```sh
scripts/add-user.sh /etc/olcrtc-manager/config.json alice --from default
scripts/modify-user.sh /etc/olcrtc-manager/config.json alice --location-name Germany --room-prefix alice-room
scripts/delete-user.sh /etc/olcrtc-manager/config.json alice
```

`add-user.sh` без `--room` генерирует только Jitsi-комнаты. Для `wbstream` и `telemost` комнату нужно создать вручную у провайдера и передать её через `--room`.

Передайте `--reload http://127.0.0.1:8888/-/reload`, чтобы перезагрузить работающий менеджер после сохранения конфигурации.
