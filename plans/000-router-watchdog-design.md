# PLAN: `yarddog` — watchdog для GPON-роутера Nokia G-240W-A

Имя проекта: `yarddog` (зафиксировано). Язык кода: Go, только stdlib + `modernc.org/sqlite`. Комментарии в
коде — на английском, первое слово с маленькой буквы.

Хост: Raspberry Pi 5 (Ubuntu Server, arm64), подключён к роутеру по проводу.
Запуск: cron, два расписания.

---

## 1. Цель и режимы

Одна CLI-утилита, два режима запуска:

| Режим | Запуск | Поведение |
|---|---|---|
| **soft** | без аргументов, cron каждые 30 мин | проверить интернет; если есть — записать результат в БД и выйти; если нет — перезагрузить роутер |
| **hard** | `--hard-reboot`, cron ежедневно в 04:00 | безусловная перезагрузка роутера |

Оба режима после запуска ребута переходят в общий **recovery loop**: висят и
проверяют те же цели раз в минуту, фиксируя фазы «роутер погас» → «роутер
поднялся» → «интернет восстановлен», с уведомлениями в Telegram.

Всё (каждая проверка, каждый запуск, каждая фаза ребута) пишется в SQLite.

---

## 2. CLI

```
yarddog                  # soft-режим
yarddog --hard-reboot    # hard-режим
yarddog --env /etc/yarddog.env   # путь к .env (default: /etc/yarddog.env, fallback ./.env)
```

Возможное расширение (не MVP): `--last N` — вывести последние N запусков из БД
в терминал (удобно по SSH), `--check-only` — только проверка без ребута.

Exit-коды:

| Код | Значение |
|---|---|
| 0 | ок (интернет есть / ребут успешно завершён) |
| 1 | ошибка конфигурации (.env, DSN) |
| 2 | другой инстанс уже работает (lock занят) |
| 3 | ребут не удался (login/reboot.cgi) |
| 4 | ребут прошёл, но интернет не восстановился за таймаут |
| 5 | ребут пропущен из-за cooldown (см. п.6) |

---

## 3. Конфигурация (.env)

Собственный мини-загрузчик `KEY=VALUE` (~20 строк, без зависимостей): пропуск
пустых строк и `#`-комментариев, снятие кавычек. Значения из реального окружения
имеют приоритет над файлом.

| Переменная | Обязат. | Default | Описание |
|---|---|---|---|
| `LABEL` | да | — | метка в сообщениях: `#REBOOT {LABEL}` |
| `TELEGRAMBOT_DSN` | да | — | `tbot://{chat_id}:@{bot_token}/` (см. п.8.1) |
| `ROUTER_ADDR` | нет | `http://192.168.1.1` | адрес веб-интерфейса |
| `ROUTER_USER` | да | — | логин админки |
| `ROUTER_PASS` | да | — | пароль админки |
| `DB_PATH` | нет | `/var/lib/yarddog/yarddog.db` | путь к SQLite |
| `CHECK_IPS` | нет | `1.1.1.1:443,8.8.8.8:53` | цели по IP, csv `host:port` |
| `CHECK_DOMAINS` | нет | `https://www.google.com/generate_204,https://cloudflare.com/cdn-cgi/trace` | цели по доменам (проверяют и DNS) |
| `CHECK_TIMEOUT` | нет | `5s` | таймаут одной проверки |
| `RECOVERY_INTERVAL` | нет | `60s` | период опроса в recovery loop |
| `RECOVERY_TIMEOUT` | нет | `15m` | сколько ждать восстановления после ребута |
| `REBOOT_COOLDOWN` | нет | `2h` | не делать повторный soft-ребут раньше этого срока |
| `RETENTION_DAYS` | нет | `90` | чистка старых `checks` (0 = не чистить) |

Файл `/etc/yarddog.env` — `root:root`, `chmod 600` (там пароль роутера и токен бота).

---

## 4. Проверка интернета

### 4.1. Метод «пинга» — решение

Настоящий ICMP-ping в Go требует raw-сокетов (root или
`setcap cap_net_raw`). Вместо этого — **TCP-dial + HTTP**, что для задачи
эквивалентно и не требует привилегий и зависимостей:

- **по IP**: `net.DialTimeout("tcp", "1.1.1.1:443", timeout)` и `8.8.8.8:53` —
  проверяет чистую IP-связность, минуя DNS;
- **по доменам**: `GET https://www.google.com/generate_204` (ждём 204) и
  `GET https://cloudflare.com/cdn-cgi/trace` (ждём 200) — проверяет DNS-резолв +
  реальный HTTP-путь.

Если принципиален именно ICMP — вариант Б: `golang.org/x/net/icmp` +
`setcap cap_net_raw+ep` на бинарь. По умолчанию — вариант А (TCP/HTTP).

### 4.2. Кворум — когда интернет считается «упавшим»

- `ip_down`  = **все** IP-цели провалились;
- `dns_down` = **все** доменные цели провалились (при живых IP → завис DNS-путь,
  что тоже лечится ребутом роутера);
- интернет **DOWN** ⇔ `ip_down OR dns_down`.

Одна упавшая цель из группы — не триггер (защита от ложных ребутов, когда лёг
конкретный сервер). Проверки внутри группы идут параллельно (goroutine на цель),
латентность каждой пишется в БД.

---

## 5. Общая последовательность ребута (для обоих режимов)

Состояния и сообщения:

```
[flush outbox]                     — дослать неотправленные сообщения (п.8.3)
SEND  "starting router reboot"     — или в outbox, если сети нет
POST  login.cgi -> reboot.cgi      — из v1 (sid/lsid cookie, "done reboot")
loop каждые RECOVERY_INTERVAL (60s), максимум RECOVERY_TIMEOUT (15m):
    gw   = TCP-dial ROUTER_ADDR (жив ли роутер)
    inet = проверки из п.4 (те же цели)
    запись в checks (phase='recovery')
    переходы:
      роутер отвечал -> перестал:  SEND "router went down"
      роутер не отвечал -> ответил: SEND "router is up, waiting for internet"
      inet OK:                      SEND "reboot completed, internet restored
                                          (downtime 4m10s)" -> выход 0
по таймауту: SEND "internet still down after 15m, giving up" -> выход 4
```

Нюансы:
- если роутер перезагрузился так быстро, что фаза DOWN не была замечена между
  опросами — сообщения down/up пропускаются, шлётся только финальное;
- downtime считается от `reboot_started_at` до `internet_restored_at`;
- логаут в роутер не делаем — сессия умирает вместе с ребутом.

## 6. Логика soft-режима

```
проверка интернета (п.4), запись в runs+checks
интернет есть  -> action='none', выход 0
интернет нет   -> смотрим в БД последний run с action='reboot':
    моложе REBOOT_COOLDOWN -> action='skipped_cooldown',
        SEND/outbox "no internet, skipping reboot (cooldown: last reboot 40m ago)",
        выход 5
    старше -> последовательность ребута (п.5), reason="no internet"
```

**Зачем cooldown**: при аварии у провайдера (не в роутере) интернет не вернётся
после ребута — без cooldown утилита перезагружала бы роутер каждые 30 минут всю
аварию. Hard-режим cooldown игнорирует (он безусловный по определению).

## 7. Логика hard-режима

Без проверки: сразу последовательность ребута (п.5), reason="scheduled hard
reboot". Если интернет в этот момент уже лежал — сообщения уйдут через outbox
после восстановления.

---

## 8. Telegram

### 8.1. DSN

Формат: `TELEGRAMBOT_DSN="tbot://115818690:@{TBOT_TOKEN}/"`.

**Принятая трактовка** (⚠️ подтвердить): `tbot://{chat_id}:@{bot_token}/`, где
`chat_id=115818690` (личный chat id получателя), `bot_token` — полный токен вида
`NNNNNNNNN:AAAA...`. Парсинг ручной (стандартный `net/url` не переварит `:`
внутри токена):

```go
// parseDSN splits "tbot://{chat}:@{token}/" into chat id and bot token.
rest, ok := strings.CutPrefix(dsn, "tbot://")
chat, token, ok := strings.Cut(strings.TrimSuffix(rest, "/"), ":@")
```

### 8.2. Отправка

`POST https://api.telegram.org/bot{token}/sendMessage`,
`{"chat_id": ..., "text": ...}`, таймаут 10s, stdlib `net/http`. Без библиотек.

### 8.3. Outbox — сообщения при лежащем интернете

Ключевая проблема: у Pi единственный аплинк — через этот же роутер. В soft-режиме
ребут по определению начинается **без интернета** → «starting reboot» доставить
нельзя. Решение — паттерн outbox поверх той же SQLite:

- каждое сообщение сначала пишется в `tg_outbox`;
- сразу пробуем отправить; успех → `sent_at`, ошибка → остаётся в очереди;
- очередь дожимается: в конце recovery loop (интернет вернулся) и в начале
  каждого следующего запуска;
- к сообщению, отправленному с опозданием, добавляется исходное время:
  `... [queued 04:02]`.

### 8.4. Шаблоны сообщений (английский, тег в начале)

```
#REBOOT {LABEL} starting router reboot (reason: no internet)
#REBOOT {LABEL} starting router reboot (reason: scheduled hard reboot)
#REBOOT {LABEL} router went down
#REBOOT {LABEL} router is up, waiting for internet
#REBOOT {LABEL} reboot completed, internet restored (downtime 4m10s)
#REBOOT {LABEL} no internet, skipping reboot (cooldown: last reboot 40m ago)
#REBOOT {LABEL} reboot failed: <error>
#REBOOT {LABEL} internet still down after 15m, giving up
```

---

## 9. SQLite

Драйвер: `modernc.org/sqlite` — чистый Go, без cgo → кросс-компиляция
Mac→arm64 без боли. `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000`.
Времена — UTC, RFC3339. Миграция — `CREATE TABLE IF NOT EXISTS` при старте.

```sql
CREATE TABLE IF NOT EXISTS runs (
  id                   INTEGER PRIMARY KEY,
  started_at           TEXT NOT NULL,
  mode                 TEXT NOT NULL,   -- 'soft' | 'hard'
  internet_ok          INTEGER,         -- результат стартовой проверки (NULL в hard)
  action               TEXT NOT NULL,   -- 'none' | 'reboot' | 'skipped_cooldown'
  reboot_started_at    TEXT,
  router_down_at       TEXT,
  router_up_at         TEXT,
  internet_restored_at TEXT,
  finished_at          TEXT,
  outcome              TEXT,            -- 'ok'|'reboot_failed'|'timeout'|'skipped'
  error                TEXT
);

CREATE TABLE IF NOT EXISTS checks (
  id         INTEGER PRIMARY KEY,
  run_id     INTEGER NOT NULL REFERENCES runs(id),
  ts         TEXT NOT NULL,
  phase      TEXT NOT NULL,   -- 'initial' | 'recovery'
  target     TEXT NOT NULL,   -- '1.1.1.1:443' | 'https://...' | 'gateway'
  kind       TEXT NOT NULL,   -- 'ip' | 'domain' | 'gateway'
  ok         INTEGER NOT NULL,
  latency_ms INTEGER,
  error      TEXT
);
CREATE INDEX IF NOT EXISTS idx_checks_run ON checks(run_id);
CREATE INDEX IF NOT EXISTS idx_checks_ts  ON checks(ts);

CREATE TABLE IF NOT EXISTS tg_outbox (
  id         INTEGER PRIMARY KEY,
  created_at TEXT NOT NULL,
  text       TEXT NOT NULL,
  sent_at    TEXT,
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT
);
```

Объём: soft-прогон каждые 30 мин × ~5 целей ≈ 240 строк/сутки — копейки; чистка
`checks` старше `RETENTION_DAYS` при старте.

---

## 10. Cron, блокировка, коллизии

### 10.1. Коллизия в 04:00

`*/30` срабатывает и в 04:00 — одновременно с hard-ребутом. Решение двухслойное:

1. **сместить soft-расписание на минуты 7 и 37** — коллизия исчезает по построению;
2. **flock** на `/var/run/yarddog.lock` (`syscall.Flock`, `LOCK_EX|LOCK_NB`) —
   защита от наложения вообще (recovery loop живёт до 15 минут и может встретить
   следующий cron-тик). Lock снимается ядром при смерти процесса — stale-lock
   невозможен. Занято → лог + выход 2.

### 10.2. Crontab (root)

```cron
7,37 * * * *  /usr/local/bin/yarddog >> /var/log/yarddog.log 2>&1
0 4  * * *    /usr/local/bin/yarddog --hard-reboot >> /var/log/yarddog.log 2>&1
```

stderr/stdout — дублирующий плоский лог; первичная история — SQLite. Проверить,
что TZ на Pi = `Asia/Almaty` (04:00 должно быть локальными).

---

## 11. Структура проекта и зависимости

Плоский `package main`, файлы по зонам ответственности (для утилиты такого
размера пакеты — оверинжиниринг):

```
yarddog/
  main.go        # CLI, режимы, exit-коды, flock
  config.go      # структура конфига, валидация
  env.go         # мини-загрузчик .env
  check.go       # цели, параллельные проверки, кворум
  router.go      # login/reboot клиент (перенос из v1)
  telegram.go    # DSN-парсер, sendMessage, outbox flush
  store.go       # sqlite: миграции, runs/checks/outbox, cooldown-запрос
  run.go         # оркестратор: soft/hard, recovery state machine
  *_test.go
```

Зависимости: `modernc.org/sqlite`. Всё остальное — stdlib.

Интерфейсы для тестируемости: `checker`, `rebooter`, `notifier`, `clock` —
recovery loop принимает их и тестируется без сети и сна.

---

## 12. Тесты

Гейт после каждой итерации: `go vet ./... && go test ./...`.

- `env_test.go` — парсер .env: кавычки, комментарии, приоритет реального env;
- `telegram_test.go` — DSN: валидный, без префикса, без `:@`, пустой токен;
  отправка через `httptest` (проверка chat_id/text в запросе); outbox: ошибка →
  осталось в очереди → flush дослал с пометкой `[queued ...]`;
- `check_test.go` — кворум: одна цель упала ≠ down; все IP упали = down; IP живы,
  все домены упали = down; латентность пишется;
- `router_test.go` — перенос из v1: login ставит sid, bad creds, reboot ok /
  not confirmed, sid уходит во второй запрос;
- `store_test.go` — на `:memory:`: миграции идемпотентны, cooldown-запрос
  (последний ребут моложе/старше порога), retention;
- `run_test.go` — state machine на фейковых clock/checker: happy path
  (down→up→inet ok, правильный порядок сообщений), быстрый ребут без фазы down,
  таймаут → exit 4, cooldown → exit 5.

---

## 13. Деплой

1. `go build -o yarddog .` (на Pi или кросс-компиляцией `GOOS=linux GOARCH=arm64`);
2. `sudo mv yarddog /usr/local/bin/`;
3. `sudo mkdir -p /var/lib/yarddog` (владелец — пользователь cron);
4. `/etc/yarddog.env` (chmod 600) — заполнить LABEL, DSN, ROUTER_*;
5. дымовой прогон руками: `sudo yarddog` при живом интернете (ожидаем: запись в
   runs, action='none', exit 0), затем `sudo yarddog --hard-reboot` — полный цикл
   с сообщениями в Telegram и обнулением Device Running Time на роутере;
6. добавить cron-строки (п.10.2);
7. через сутки — глянуть runs/outbox: `sqlite3 /var/lib/yarddog/yarddog.db
   'select started_at,mode,action,outcome from runs order by id desc limit 10;'`.

---

## 14. Риски и краевые случаи

- **Авария у провайдера** → без cooldown ребут-петля каждые 30 мин; закрыто п.6.
- **Telegram недоступен, когда сообщение нужнее всего** → outbox (п.8.3).
- **Наложение инстансов** (recovery 15 мин > интервал 30 мин при деградации,
  коллизия 04:00) → flock + смещение минут (п.10).
- **Роутер жив, но админка не отвечает** → login провалится, outcome
  `reboot_failed`, сообщение уйдёт; лечится только физическим передёргиванием.
  Возможное развитие: умная розетка (Tapo/Sonoff) как «жёсткий» fallback.
- **32-байтное тело reboot.cgi** — из v1: стартуем с пустым телом, при
  `reboot not confirmed` вставить снятые байты в константу.
- **Прошивка обновится, эндпоинты сменятся** → reboot_failed, увидим в Telegram.
- **Питание Pi пропало посреди recovery** → flock снят ядром, run останется без
  finished_at (видно в БД как оборванный) — приемлемо.
- **Ложный DOWN из-за пары потерянных пакетов** → кворум «все цели группы» +
  таймаут 5s; при желании ужесточается до «две волны проверок с паузой 10s»
  (опция на будущее, не MVP).

## 15. Открытые вопросы (подтвердить до кода)

1. **DSN**: трактовка `tbot://{chat_id}:@{bot_token}/` верна? 115818690 — это
   chat id получателя?
2. **Метод проверки**: TCP/HTTP-«пинг» ок, или принципиален ICMP (+setcap)?
3. **Пороги**: cooldown 2h, recovery timeout 15m, интервал 60s — норм?
4. **Минуты крона**: 7/37 вместо 0/30 — ок?
5. **ROUTER_USER** фактический (из v1, не подтверждён).
6. ~~Имя проекта~~ — решено: `yarddog`.

## 16. Чеклист исполнения

- [ ] Закрыть открытые вопросы (п.15)
- [ ] Скелет: config + env loader + тесты
- [ ] store.go: миграции + тесты на :memory:
- [ ] check.go: проверки + кворум + тесты
- [ ] router.go: перенос из v1 + тесты
- [ ] telegram.go: DSN + send + outbox + тесты
- [ ] run.go: state machine + тесты на фейковом clock
- [ ] main.go: CLI, flock, exit-коды
- [ ] `go vet ./... && go test ./...` — зелёные
- [ ] Сборка arm64, деплой, .env (600), дымовые прогоны (п.13.5)
- [ ] Cron-строки, проверка TZ
- [ ] Через сутки — ревизия runs/outbox
