# map-service

Self-hosted картографический стек для **одного города** на данных OpenStreetMap: векторные тайлы, геокодинг, автодополнение адресов и маршруты за единым API-шлюзом на Go. Поднимается одной командой на одном Linux-сервере с Docker; рассчитан на мобильный клиент (Flutter + MapLibre), но от клиента ничем не зависит.

Эталонная конфигурация обслуживает **Астану** (Казахстан) из экстракта Geofabrik по стране.

```
Flutter-клиент
     │  HTTPS
     ▼
Caddy (TLS, reverse proxy)                       ← единственное, что слушает 80/443
     │  /v1/trips*
     ├──────────► trips (Go) ── поездки: позиции водителя, сход с маршрута, перестроение ──► trips-db, osrm-car
     ▼  всё остальное
gateway (Go) ── auth, rate limit, CORS, кэш, нормализация ответов, PMTiles, стиль
     ├──► photon      автодополнение   ┐
     ├──► nominatim   геокод/reverse   │  внутренняя docker-сеть
     ├──► osrm-car    маршруты         │  (без выхода в интернет, без портов на хосте)
     ├──► osrm-foot   маршруты         │
     └──► redis       кэш              ┘
updater (Go) ── скачать → вырезать город → OSRM / тайлы / Nominatim / Photon → smoke-тесты → переключение
```

* [Требования](#требования)
* [Быстрый старт](#быстрый-старт)
* [Конфигурация](#конфигурация)
* [API](#api)
* [Стиль, тайлы и Flutter-клиент](#стиль-тайлы-и-flutter-клиент)
* [Стили карты: выбор и дефолтный стиль](#стили-карты-выбор-и-дефолтный-стиль)
* [Пайплайн данных и обновления](#пайплайн-данных-и-обновления)
* [Версии компонентов и почему они такие](#версии-компонентов-и-почему-они-такие)
* [Принятые решения](#принятые-решения)
* [Эксплуатация](#эксплуатация)
* [Безопасность](#безопасность)
* [Тесты](#тесты)
* [Другой город и другой профиль](#другой-город-и-другой-профиль)
* [Известные ограничения](#известные-ограничения)
* [Лицензии и атрибуция](#лицензии-и-атрибуция)

## Требования

| | |
|---|---|
| Сервер | Linux, x86-64 или arm64, **4 vCPU / 8 ГБ RAM / 50 ГБ SSD** |
| ПО | Docker Engine ≥ 20.10 с плагином Compose v2, GNU make |
| Сеть | Исходящий интернет для первой сборки и обновлений (образы, экстракт OSM). **В рантайме у бэкендов выхода в интернет нет совсем.** Входящие 80/443 для публичного адреса. |
| Домен | Необязателен. Если DNS-имя указывает на сервер, Caddy сам получит сертификат Let's Encrypt. |

Проверить заготовку сервера можно `scripts/check-prereqs.sh`.

Что стек реально потребляет для Астаны (замерено в Docker-VM 5 CPU / 8 ГБ, arm64):

| | |
|---|---|
| RAM, весь стек в простое | **≈ 0,7–1,0 ГиБ** (Nominatim 160–420 МиБ, Photon ≈ 540 МиБ, OSRM ≈ 30–50 МиБ каждый, Redis ≈ 6 МиБ, gateway ≈ 15 МиБ, trips ≈ 8 МиБ, trips-db ≈ 35 МиБ) |
| RAM, пик при обновлении | **≈ 4,5 ГиБ** суммарно; из них 3,5 ГиБ — `osmium extract` на файле страны 224 МБ (см. [ограничения](#известные-ограничения)); сам импорт Nominatim пикует на ≈ 550 МиБ |
| Диск на версию данных | **≈ 0,9 ГБ**: 146 МБ в `data/<v>` (экстракт города 7 МБ, OSRM 107 МБ для car+foot, тайлы 6 МБ, Photon 25 МБ) и ≈ 750 МБ том PostgreSQL Nominatim. По умолчанию хранятся две версии (≈ 1,8 ГБ) плюс кэш экстракта страны (224 МБ). |
| Полная сборка данных | **≈ 4–6 минут** (extract 15–20 с, OSRM 19 с, тайлы 5 с, импорт Nominatim 80–100 с, импорт Photon 30 с, smoke 15 с, переключение 90 с) |

## Быстрый старт

```bash
cp .env.example .env
$EDITOR .env           # минимум: API_KEYS, NOMINATIM_PASSWORD, TRIPS_DB_PASSWORD, SITE_ADDRESS, PUBLIC_BASE_URL
make init-data         # скачивает экстракт и собирает все данные (≈ 5 мин для Астаны)
make up                # поднимает стек
curl -s localhost/readyz
```

`make init-data` собирает первую версию данных (`data/<метка времени>/`), прогоняет на ней smoke-тесты и делает её живой (`data/current`). `make up` без неё не стартует.

Локальный запуск без TLS: оставьте `SITE_ADDRESS=:80` и `PUBLIC_BASE_URL=http://localhost`. Продакшн: `SITE_ADDRESS=maps.example.com`, `PUBLIC_BASE_URL=https://maps.example.com` (последнее попадает в `style.json`, поэтому должно совпадать с адресом, по которому реально ходят клиенты).

Карта: `http://localhost/tiles/preview.html`. Это не только просмотр тайлов, но и песочница для API: поиск с автодополнением (Enter — геокодинг), клик по карте показывает адрес точки (`/v1/reverse`), кнопка **Route** включает построение маршрута (клики ставят A, B и промежуточные точки, маркеры перетаскиваются, для двух точек показываются альтернативы и пошаговые инструкции; профили `car`/`foot`). Для `/v1/*` нужен ключ: откройте страницу с `?key=<ключ из API_KEYS>` или введите его в поле на панели.

Документация API (Swagger UI с «Try it out»): `http://localhost/docs`; нажмите **Authorize** и введите ключ из `API_KEYS`, чтобы вызывать `/v1/*` прямо со страницы. Спецификация — `http://localhost/openapi.yaml` (`servers` в ней подставляется под адрес, с которого её открыли).

| Цель make | |
|---|---|
| `make init-data` | первичная загрузка и сборка всех данных |
| `make up` / `make down` | запуск / остановка стека (данные и тома сохраняются) |
| `make up-dev` | запуск с пробросом портов бэкендов на `127.0.0.1` для отладки (никогда на публичном сервере) |
| `make update-data` | пересобрать данные и переключиться на них; тот же пайплайн, что у `init-data` |
| `make test` | unit-тесты (`-race`) |
| `make test-trips-db` | тесты хранилища и API поездок на временном контейнере PostgreSQL |
| `make test-integration` | интеграционные тесты против поднятого стека |
| `make lint` | `go vet` + `golangci-lint` |
| `make logs S=gateway` / `make ps` | логи сервиса / состояние |
| `make style-assets` | пересоздать стили (`deploy/tiles/styles`), глифы и спрайты из закреплённых источников и тем (результат закоммичен) |

### Исходный экстракт

`OSM_SOURCE_URL` принимает `http(s)://`-URL (по умолчанию Geofabrik `kazakhstan-latest.osm.pbf`; кэшируется в `data/cache` и перепроверяется по `ETag`), `file://`-URL или абсолютный путь. Поскольку updater работает в контейнере, **локальный файл должен лежать внутри каталога репозитория**, например `data/sources/kazakhstan.osm.pbf` и `OSM_SOURCE_URL=file:///srv/map-service/data/sources/kazakhstan.osm.pbf`.

## Конфигурация

Вся конфигурация — переменные окружения; все они с комментариями и значениями по умолчанию перечислены в [`.env.example`](.env.example). Значения без кавычек, комментарии — отдельными строками (файл читают compose, make и Go-сервисы, и они по-разному относятся к кавычкам). Тест `test/repo` проверяет, что каждая переменная, которую читает код, описана в `.env.example`, а сам пример — валидная конфигурация.

| Группа | Переменные |
|---|---|
| Город / данные | `CITY_NAME`, `CITY_BBOX` (с буфером 10–15 км), `CITY_CENTER`, `OSM_SOURCE_URL`, `EXTRACT_MODE` (`bbox`\|`polygon`), `CITY_POLYGON_FILE`, `EXTRACT_STRATEGY`, `ROUTING_PROFILES`, `LANGUAGES` |
| Compose | `COMPOSE_PROJECT_NAME`, `COMPOSE_PROFILES` (`scheduler`, `bicycle`), `DATA_DIR` |
| Публичный адрес | `SITE_ADDRESS`, `PUBLIC_BASE_URL` |
| Gateway | `API_KEYS`, `ADMIN_API_KEYS`, `DEFAULT_STYLE`, `AUTH_REQUIRE_FOR_TILES`, `AUTH_DISABLED`, `CORS_ALLOWED_ORIGINS`, `RATE_LIMIT_{KEY,IP,TILES}_{RPS,BURST}`, `TRUSTED_PROXY_CIDRS`, `CACHE_TTL_{REVERSE,GEOCODE,AUTOCOMPLETE,ROUTE}`, `REDIS_ADDR/PASSWORD/DB`, `PHOTON_URL`, `NOMINATIM_URL`, `OSRM_URL_TEMPLATE`, `UPSTREAM_TIMEOUT`, `ROUTE_TIMEOUT`, `MAX_RESULTS_LIMIT`, `MAX_ROUTE_POINTS`, `TILES_CACHE_MAX_AGE`, `GATEWAY_LOG_LEVEL`, таймауты HTTP-сервера, `DATA_VERSION_POLL` |
| Бэкенды | `NOMINATIM_PASSWORD`, `NOMINATIM_THREADS`, `NOMINATIM_GUNICORN_WORKERS`, `POSTGRES_*`, `NOMINATIM_IMPORT_WIKIPEDIA`, `PHOTON_JAVA_OPTS`, `PHOTON_IMPORT_JAVA_OPTS` |
| База поездок | `TRIPS_DB_PASSWORD`, `TRIPS_DB_SHARED_BUFFERS` |
| Updater | `UPDATE_CRON` (UTC, по умолчанию `0 3 * * 0`), `DATA_KEEP_VERSIONS` (по умолчанию 2), `NOMINATIM_IMPORT_TIMEOUT`, `SWITCH_TIMEOUT`, `UPDATER_GATEWAY_URL`, `SMOKE_GEOCODE_QUERY`, `SMOKE_GEOCODE_MIN_RESULTS` |

Gateway не стартует с пустым `API_KEYS` (кроме явного `AUTH_DISABLED=true` — только для локальной разработки) и сообщает **обо всех** ошибках конфигурации сразу.

### Параметры, оставленные по умолчанию

Значения, выбранные самим проектом там, где в задании параметр не задан (все в `.env.example`, помечены `[DEFAULT]`): `CITY_BBOX=71.04,50.74,71.96,51.46` (административная граница Астаны — OSM relation 3087155 — плюс ≈ 12 км), `CITY_CENTER=71.4306,51.1283`, `EXTRACT_STRATEGY=smart`, порядок `LANGUAGES` — `ru,kk,en`, лимиты (20 rps на ключ, 10 rps на IP, 200 rps на IP для тайлов), `UPDATE_CRON=0 3 * * 0`, `DATA_KEEP_VERSIONS=2`, размеры памяти Redis/PostgreSQL/JVM под 8 ГБ, данные важности Wikipedia **выключены**.

## API

Полный контракт: [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1; шлюз отдаёт его на `/openapi.yaml`, интерактивно — на `/docs`). Все вызовы `/v1/*` требуют `X-API-Key`; `/healthz`, `/readyz`, `/docs` и `/tiles/*` — нет (если не включён `AUTH_REQUIRE_FOR_TILES=true`). Ответы — собственный JSON; ответы upstream наружу не отдаются.

```bash
export B=http://localhost K=your-api-key
```

**Автодополнение** — Photon; если Photon недоступен или ничего не нашёл, fallback на Nominatim. Поле `source` показывает, кто ответил.

```bash
curl -s -G -H "X-API-Key: $K" $B/v1/autocomplete \
  --data-urlencode 'q=хан ш' -d 'lat=51.13&lon=71.43&limit=3&lang=ru'
```
```json
{"places":[{"id":"way/460703779","name":"Хан Шатыр","address":{"street":"проспект Туран","house_number":"37","district":"район Нура","city":"Астана","postcode":"Z19Y4X2"},"lat":51.1325048,"lon":71.4038607,"type":"mall","source":"photon"}]}
```

**Геокод** и **reverse** — Nominatim, ограничены bbox города:

```bash
curl -s -G -H "X-API-Key: $K" $B/v1/geocode --data-urlencode 'q=Хан Шатыр' -d 'lang=kk&limit=2'
curl -s -H "X-API-Key: $K" "$B/v1/reverse?lat=51.1283&lon=71.4306&lang=ru"
```

**Маршрут** — `points` это `lon,lat;lon,lat[;…]` (`;` можно слать и как есть, и как `%3B`), `geometry` — `polyline` (по умолчанию), `polyline6` или `geojson`:

```bash
curl -s -H "X-API-Key: $K" \
  "$B/v1/route?profile=car&points=71.4306,51.1283;71.4700,51.1800&steps=true&alternatives=false"
```
```json
{"routes":[{"distance_m":9044.8,"duration_s":750.2,"geometry":"s`awHao~rLUJOL…","legs":[{"distance_m":9044.8,"duration_s":750.2,"summary":"…","steps":[{"distance_m":…,"duration_s":…,"name":"…","maneuver":{"type":"depart","lat":51.1283,"lon":71.4306},"geometry":"…"}]}]}],
 "waypoints":[{"name":"","lat":51.128581,"lon":71.431694},{"name":"…","lat":51.180024,"lon":71.469941}]}
```

**Тайлы, стиль, пробы**

```bash
curl -s $B/healthz                                  # liveness
curl -s $B/readyz                                   # все upstream + архив тайлов; 503, если что-то лежит
curl -s -o tile.mvt -D - $B/tiles/14/11442/5475.mvt # gzip-MVT с ETag и Cache-Control
curl -s $B/tiles/style.json                         # дефолтный стиль
curl -s $B/tiles/styles/positron.json               # конкретный стиль
```

**Стили** (подробнее — [ниже](#стили-карты-выбор-и-дефолтный-стиль))

```bash
curl -s -H "X-API-Key: $KEY" $B/v1/styles
curl -s -X PUT -H "X-API-Key: $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"id":"warm"}' $B/v1/styles/default
```
```json
{"default":"warm","styles":[{"id":"dark-matter",…},{"id":"liberty",…},{"id":"positron",…},
 {"id":"warm","name":"Warm","description":"…","url":"http://localhost/tiles/styles/warm.json","default":true}]}
```

Ошибки всегда вида `{"error":{"code":"…","message":"…"}}`:

| HTTP | `code` | Когда |
|---|---|---|
| 400 | `invalid_argument` | валидация: параметр пропущен/некорректен, неизвестный `profile`/`lang`, точка вне зоны обслуживания |
| 401 | `unauthorized` | нет или неверный `X-API-Key` (для `PUT /v1/styles/default` — не админский ключ) |
| 403 | `forbidden` | `PUT /v1/styles/default` при пустом `ADMIN_API_KEYS` |
| 404 | `not_found`, `no_route` | в точке ничего нет / нет такого эндпоинта / нет маршрута между точками |
| 405 | `method_not_allowed` | метод не тот (везде GET, кроме `PUT /v1/styles/default`) |
| 429 | `rate_limited` | пуст token bucket; заголовок `Retry-After` |
| 502 | `upstream_unavailable` | бэкенд недоступен или ответил мусором |
| 504 | `upstream_timeout` | бэкенд не ответил вовремя |

Бизнес-правила gateway: результаты геокодинга за пределами `CITY_BBOX` отбрасываются (и bbox/viewbox передаётся в Photon и Nominatim); точки маршрута и координаты reverse обязаны лежать внутри; `profile` — из `ROUTING_PROFILES`; `lang` — из `LANGUAGES`.

**Кэш (Redis)**: reverse — координаты, округлённые до 5 знаков, 7 суток; geocode и autocomplete — нормализованный запрос + язык (+ limit, а для автодополнения ещё смещение, округлённое до ≈ 1 км), 1 сутки; route — профиль + точки с округлением до 5 знаков + параметры, 1 час. В префиксе ключа — версия данных, поэтому после пересборки старые ответы не отдаются. Ошибки не кэшируются; не кэшируется и fallback на Nominatim, случившийся из-за *сбоя* Photon. Если Redis недоступен, gateway продолжает работать без кэша и пишет warning (раз за окно backoff).

## Стиль, тайлы и Flutter-клиент

* Тайлы: `tilemaker` (схема OpenMapTiles) пишет `data/<v>/tiles/city.pmtiles`; gateway раздаёт его встроенным `go-pmtiles` как `/tiles/{z}/{x}/{y}.mvt` (z0–14, дальше клиент делает overzoom). Тайлы всегда хранятся и отдаются сжатыми gzip (`Content-Encoding: gzip`) с `ETag` и `Cache-Control: public, max-age=3600`; Caddy распаковывает их только для клиента, не приславшего `Accept-Encoding: gzip`.
* Стили: OSM Liberty, Positron, Dark Matter (код BSD, дизайн CC-BY) и собственная тема `warm`; все адаптированы `cmd/stylegen`: URL указывают на gateway, сторонние CDN и растровый слой рельефа убраны, Roboto/Metropolis заменены на Noto Sans (OFL), подписи — `coalesce(name:<язык>, name:latin)` с первым языком из `LANGUAGES`. Атрибуции заданы в каждом стиле. Выбор и смена дефолтного стиля — [в следующем разделе](#стили-карты-выбор-и-дефолтный-стиль).
* Шрифты, спрайты и превью — статические файлы, вшитые в образ gateway (`deploy/tiles`).

Flutter (`maplibre_gl`): `styleString: 'https://maps.example.com/tiles/style.json'` (дефолтный стиль; конкретный — `/tiles/styles/<id>.json`, список — `GET /v1/styles`); при `AUTH_REQUIRE_FOR_TILES=true` передавайте `X-API-Key` через механизм заголовков запросов плагина. Смена языка в приложении: заменить выражение `text-field` `["coalesce",["get","name:ru"],["get","name:latin"]]` на другой `name:<язык>` (рабочий пример — `deploy/tiles/preview.html`).

## Стили карты: выбор и дефолтный стиль

Установленные стили лежат в `deploy/tiles/styles/<id>.json` (вшиты в образ gateway как `STATIC_DIR/styles`):

| id | Что это |
|---|---|
| `liberty` | OSM Liberty — цветная карта с иконками POI (стиль по умолчанию) |
| `positron` | светло-серая неконтрастная подложка под маршруты и маркеры |
| `dark-matter` | тёмная подложка для ночного режима |
| `warm` | собственная тема на основе Liberty: кремовая земля, оранжевые магистрали, белые улицы с серой обводкой, светлые здания с мягким 3D, полупрозрачные коричневые подписи районов, цвета подписей POI по категориям, номера домов с z16 |

* `GET /v1/styles` (обычный API-ключ) — список: `id`, `name`, `description`, `url`, `default`.
* `GET /tiles/styles/<id>.json` — конкретный стиль; `GET /tiles/style.json` — **дефолтный** (его и берёт мобильное приложение). Стили отдаются с `Cache-Control: no-cache` и `ETag`: клиенты видят смену дефолтного стиля (и пересобранную тему) при следующей загрузке стиля, а повторная загрузка без изменений — дешёвый `304`.
* `PUT /v1/styles/default` с телом `{"id":"<id>"}` — сделать стиль дефолтным для всех клиентов, без перезапуска. Нужен ключ из `ADMIN_API_KEYS` (обычные ключи клиентов не подходят; админские ключи работают и как обычные). Пустой `ADMIN_API_KEYS` выключает эндпоинт (`403`), `AUTH_DISABLED` его не открывает. Выбор сохраняется в томе `gateway-state` (`STATE_DIR=/state`) и переживает рестарты; до первого выбора действует `DEFAULT_STYLE` (по умолчанию `liberty`). Если выбранный стиль пропал из образа, gateway откатывается на `DEFAULT_STYLE`.
* Страница превью `/tiles/preview.html`: выпадающий список **Style** (из `GET /v1/styles`, нужен ключ в поле API key) переключает стиль на карте, `?style=<id>` открывает конкретный стиль, кнопка **Make default** вызывает `PUT /v1/styles/default` (введите админский ключ).

**Как поиграться со стилем.** Тема — это JSON поверх базового стиля: [`internal/stylegen/themes/warm.json`](internal/stylegen/themes/warm.json). В `layers` — переопределения `paint`/`layout`/`filter`/`minzoom`/`maxzoom` существующих слоёв по `id` (значение `null` удаляет свойство), в `insert` — новые слои с якорем `before`, `"text-field": "{name}"` превращается в выражение с языком. Поменяйте цвет, выполните `make style-assets` и обновите превью (`?style=warm`); пересоберите gateway (`make up`), чтобы стиль попал в образ. Новая тема — новый файл в `internal/stylegen/themes/` с уникальным `id` и `base` (`liberty`, `positron` или `dark-matter`). Генератор падает, если тема ссылается на несуществующий слой или шрифт без глифов, а `test/repo` — если закоммиченный стиль устарел относительно темы. Для визуальной правки удобен [Maputnik](https://maplibre.org/maputnik/): откройте в нём `/tiles/styles/<id>.json`, а найденные значения перенесите в тему.

Происхождение палитры `warm`: цвета (и несколько ширин линий и размеров подписей) сняты с экспорта дефолтной темы 2GIS и вручную сопоставлены со слоями OpenMapTiles (Federal → motorway, Magistral → trunk/primary/secondary, Main → tertiary, Other → minor, Internal → service/track; `secondary` считается магистралью, потому что крупные проспекты Астаны размечены в OSM именно так). Иконки, шрифты, модели и код 2GIS не используются, иконки POI — открытые Maki из OSM Liberty. В видимых клиентам названии, описании и метаданных стиля 2GIS не упоминается; пометка об источнике есть только в `notes` исходника темы.

## Пайплайн данных и обновления

`cmd/updater` выполняет шаги ниже внутри контейнера `updater`; инструменты запускаются соседними контейнерами через `docker compose run`.

| # | Шаг | Что происходит |
|---|---|---|
| 1 | prepare | собрать образ Photon, подтянуть образы инструментов |
| 2 | download | скачать экстракт страны (кэш с проверкой ETag) или взять локальный файл |
| 3 | extract | `osmium extract` по bbox (или по GeoJSON-полигону, `EXTRACT_MODE=polygon`) → `data/<v>/city.osm.pbf`; проверка результата на вменяемость |
| 4 | osrm | `osrm-extract → partition → customize` (MLD) по разу на профиль |
| 5 | tiles | `tilemaker` → `tiles/city.pmtiles` (конфиг генерируется из шаблона с bbox города) |
| 6 | nominatim | **контейнер-кандидат** Nominatim импортирует экстракт в новый именованный том `…-nominatim-pg-<v>` |
| 7 | photon | `import` Photon читает PostgreSQL кандидата → `photon/photon_data` |
| 8 | smoke | рядом с живым стеком стартуют кандидаты OSRM (по профилю) и Photon; **собственными клиентами gateway** проверяются: тайл z14 в центре, геокод, reverse, маршрут по каждому профилю, автодополнение |
| 9 | release | записать `manifest.json`, остановить кандидата, атомарно переключить `data/current`, пересоздать `osrm-*`, `photon`, `nominatim` **по одному**, дожидаясь health, затем дождаться, пока `/readyz` gateway покажет новую версию |
| 10 | cleanup | удалить старые версии (и их тома Nominatim), оставить `DATA_KEEP_VERSIONS` (включая живую) |

Гарантии (все покрыты unit-тестами на скриптованном Docker; успешный путь, падение позднего шага, хранение версий и откат дополнительно проверены на настоящем Docker):

* Сборка идёт в `data/<новая версия>` рядом с работающим стеком: **до `release` живое не трогается**.
* **Любой упавший шаг** оставляет живую версию в работе, удаляет недостроенный каталог, контейнер-кандидат и его том, а updater завершается с ненулевым кодом и строкой лога с именем шага. Упавший `release` (сервис не стал healthy, gateway не готов, не та версия) **откатывает** `data/current` и сервисы.
* Одновременно — только одно обновление (`data/.update.lock`); второе выходит с кодом 3.

Переключение — это скользящий перезапуск, а не blue/green. Замер на живом стеке (зонд дёргал каждый эндпоинт ≈ 2 раза/с с уникальными параметрами во время трёх реальных `make update-data`): фаза сборки **не вызвала ошибок**; при переключении у тайлов и автодополнения **нет ни одного неуспешного запроса** (Photon и Nominatim подстраховывают друг друга), перезапуск OSRM стоит не более ≈ 1 с `502` на маршрут, а геокод/reverse отвечали `502` ≈ 2–7 с, пока перезапускался Nominatim (`/readyz` во время каждого перезапуска — `503`). Записи кэша старой версии после переключения не читаются.

Расписание: с compose-профилем `scheduler` (включён по умолчанию в `.env.example`) сервис `scheduler` запускает `updater cron` по `UPDATE_CRON` (UTC, если не указан `CRON_TZ=`). Упавшее плановое обновление логируется и повторяется в следующее срабатывание. Уберите `scheduler` из `COMPOSE_PROFILES`, чтобы обновлять только вручную или из собственного cron/systemd-таймера:

```cron
0 3 * * 0  /srv/map-service/scripts/update-data.sh >> /var/log/map-update.log 2>&1
```

Флаги отладки updater: `updater run -only download,extract` (частичный запуск, никогда не переключает, каталог сохраняется), `-version <v>` (продолжить в существующем каталоге сборки), `-keep-on-failure`. Пример: `docker compose … run --rm updater run -only osrm -version 20261014T063746Z`.

### Почему инструменты запускаются через `docker compose run`

`osmium` собирается из закреплённых исходников в образ updater (надёжного официального образа нет); `osrm-*`, `tilemaker`, Nominatim и Photon запускаются своими закреплёнными образами через `docker compose run`. Альтернатива — запечь всё в образ updater — продублировала бы зависимости и профили этих образов, а `docker run` с вручную собранными флагами разбросал бы закрепления версий. Сервисы compose (профиль `tools`) дают то, что **все версии образов лежат в `deploy/docker-compose.yml`**, а Go-код оперирует только именами сервисов. Контейнер updater монтирует репозиторий и каталог данных **по тем же абсолютным путям, что на хосте**, поэтому пути bind-mount, которые он передаёт демону, корректны.

## Версии компонентов и почему они такие

Все версии проверены по реестрам 2026-10-14 и закреплены явно.

| Компонент | Закреплено | Почему |
|---|---|---|
| Go | 1.27.2 | последняя стабильная; маршрутизация `net/http` по паттернам, `log/slog` |
| Nominatim | `mediagis/nominatim:5.3.2` | последний патч 5.3 (исправляет ошибку ограничения при апдейтах); amd64+arm64; внутри PostgreSQL 16 |
| Photon | 1.3.0 (jar релиза, sha256 закреплён) | последний релиз. **Нужна Java 21+; backend поиска — OpenSearch 3.x, встроенный в jar** (для одного города отдельный контейнер OpenSearch не нужен). Официального образа нет, поэтому `deploy/photon/Dockerfile` оборачивает jar на `eclipse-temurin:21.0.12_8-jre-noble`. В Photon 1.x импорт из Nominatim — это подкоманда `import` (флага `-nominatim-import` больше нет); она подключается к PostgreSQL контейнера Nominatim по приватной сети |
| OSRM | `ghcr.io/project-osrm/osrm-backend:v26.10.0-debian` | последний релиз (OSRM перешёл на календарные версии; теги с суффиксом `-debian`); штатные профили `car`/`foot`/`bicycle` из `/opt`, алгоритм MLD |
| tilemaker | `ghcr.io/systemed/tilemaker@sha256:d3eda279…` (3.2.0) | **пишет PMTiles напрямую** (`pmtiles convert` не нужен). Опубликован только плавающий тег `master`, поэтому образ закреплён по digest; метка образа — `Version 3.2.0`, ревизия `facdf93` |
| go-pmtiles | v1.31.2 | последняя; встроенный серверный пакет |
| OSM Liberty | коммит `da16155` | совместим с OpenMapTiles, разрешительная лицензия; каждый скачиваемый файл закреплён по sha256 |
| Глифы Noto Sans | openmaptiles/fonts v2.0 | OFL; покрывает латиницу, кириллицу и казахские буквы Ә Ғ Қ Ң Ө Ұ Ү Һ І, а также ₸ (в репозитории только 9 диапазонов Unicode × 3 начертания, 2,4 МБ) |
| MapLibre GL JS | 5.24.0 | только для `preview.html`, вложен в репозиторий. 6.x (на момент закрепления вышел 3 дня назад) — только ES-модули с отдельным воркером |
| Swagger UI | 5.32.15 (swagger-ui-dist) | только для `/docs`, вложен в репозиторий, закреплён по sha256 (Apache-2.0). Берётся не самый свежий релиз (5.33.x вышел недавно), а предыдущая линия, которой уже больше месяца |
| Caddy | 2.11.7-alpine | последний 2.x |
| PostgreSQL (trips-db) | `postgres:18.6-alpine` | последний патч 18.x; id поездок — `uuidv7()` (есть с 18-й версии). Образы 18+ хранят данные в `/var/lib/postgresql/18/docker`, поэтому том монтируется в `/var/lib/postgresql` |
| Redis | 8.10.2-alpine | последний 8.x. Замечание: Redis 8 лицензирован ещё и по AGPLv3 / RSALv2 / SSPL; если это важно — используйте `valkey/valkey` |
| osmium-tool | 1.19.1 (libosmium 2.23.1, protozero 1.8.2), собирается из тегов | в Alpine нет пакета `osmium-tool` |
| Docker CLI / Compose в updater | Alpine 3.24: 29.8.2 / 5.1.4 | проверено против старого движка (API 1.41) |
| robfig/cron v3.0.1, go-redis v9.23.0, miniredis v2.40.0, x/time v0.16.0, pgx v5.10.0 | | разбор cron, клиент Redis, тесты, token bucket, драйвер PostgreSQL |

## Принятые решения

* **Изолированная сеть.** `internal` — Docker-сеть с `internal: true`: Nominatim, Photon, OSRM и Redis не выходят в интернет и ничего не публикуют. Наружу смотрит только Caddy (80/443). `updater` подключён к обеим сетям, потому что скачивает данные.
* **Один том Nominatim на версию данных** (`<проект>-nominatim-pg-<версия>`): новый импорт не трогает базу, которая обслуживает трафик, откат — это переключение симлинка, очистка — `docker volume rm`.
* **Исключение по доступу к Postgres.** Gateway никогда не подключается к PostgreSQL. К базе Nominatim подключается только контейнер *импорта* Photon делает это по приватной сети под ролью `nominatim` с паролем `NOMINATIM_PASSWORD`.
* **База поездок отдельно от карты.** `trips-db` (PostgreSQL) хранит поездки, все версии их маршрутов, каждую присланную позицию с вердиктом трекера и журнал событий для наблюдателей. Это пользовательские данные: updater её не трогает, а бэкапить её надо (см. эксплуатацию), в отличие от кэша Redis и тома Nominatim. Пишет в неё только сервис `trips` (`cmd/trips`). Схема — миграции `internal/trips/store/migrations`, их применяет сам сервис при старте (параллельные запуски сериализует advisory lock). Все записи одной поездки идут под блокировкой её строки, поэтому события поездки фиксируются строго по порядку их id, и переподключившийся наблюдатель по `Last-Event-ID` ничего не теряет.
* **Gateway сам следит за живой версией.** Он идёт по симлинку `data/current` (тайлы — на каждый запрос, метка версии — раз в 10 с), поэтому переключение не требует перезапуска gateway.
* **Стратегия `smart`** сохраняет целиком мультиполигоны (озёра, землепользование) и границы через край bbox; цена — больше памяти (см. ограничения) и экстракт, чей bbox шире города, поэтому tilemaker получает явный `bounding_box`.
* **Разбор query.** `net/url` в Go молча выбрасывает пары query, содержащие `;`; `points=lon,lat;lon,lat` просто пропадал бы. Gateway разбирает сырой query сам.
* **Логи.** JSON через `log/slog`, `request_id` в каждой строке (из корректного `X-Request-Id` либо сгенерированный), API-ключи — только короткий хэш, строки запроса не логируются (в них адреса). Лог Caddy (`docker logs`/`make logs S=caddy`) по умолчанию пишет весь URI и заголовки, поэтому `deploy/Caddyfile` подменяет ключ на `REDACTED` в заголовке `X-API-Key`, параметре `?key=` (страница превью) и в `Referer`, в строках запросов и ошибок; остальные параметры запроса (адреса, координаты) в логе Caddy остаются. Правило проверяет тест `test/repo`.

## Эксплуатация

```bash
make ps                               # состояние
make logs S=gateway                   # логи сервиса
docker compose -p mapsvc -f deploy/docker-compose.yml --env-file .env logs scheduler
cat data/current/manifest.json        # из чего собрана живая версия и сколько шло каждый шаг
du -sh data/*                         # диск по версиям
```

* Бэкапы: данные карты воспроизводимы из OSM, поэтому сохранять стоит `.env` и — чтобы не перестраивать большой город часами — каталог `data/` вместе с томом `…-nominatim-pg-<текущая версия>`.
* База поездок не воспроизводима, её бэкап обязателен: `docker compose -p mapsvc -f deploy/docker-compose.yml --env-file .env exec -T trips-db pg_dump -U trips -Fc trips > trips-$(date +%F).dump` (восстановление — `pg_restore`). Данные лежат в томе `…_trips-db`.
* Том Caddy (`…_caddy-data`) хранит TLS-сертификаты; сохраняйте его между перезапусками.
* Ручной откат: `ln -sfn <старая версия> data/current` и `docker compose … up -d --force-recreate osrm-car osrm-foot photon nominatim`.
* Логи всех контейнеров ротируются (10 МБ × 3).

## Безопасность

* Сервисы `scheduler` и `updater` монтируют `/var/run/docker.sock`, что **эквивалентно root на хосте**. Они исполняют только доверенный код из этого репозитория, но если это неприемлемо — уберите `scheduler` из `COMPOSE_PROFILES` и запускайте `make update-data` из таймера под вашим контролем.
* Замените `API_KEYS`, `NOMINATIM_PASSWORD` и `TRIPS_DB_PASSWORD`. Ключи сравниваются за константное время; лимиты действуют на ключ и на IP (`X-Forwarded-For` принимается только от `TRUSTED_PROXY_CIDRS`, т. е. от Caddy).
* `/readyz` публичен, но никогда не печатает внутренние адреса и тексты ошибок.
* `deploy/docker-compose.dev.yml` публикует порты только на `127.0.0.1`; в продакшне не использовать.
* Смена дефолтного стиля — только с ключом из `ADMIN_API_KEYS`, каждая смена пишется в лог (`default style changed`, без ключа).
* Страница превью и стили не обращаются ни к каким сторонним хостам; `X-API-Key` для тайлов необязателен (`AUTH_REQUIRE_FOR_TILES`).

## Тесты

```bash
make test               # 323 тестовых случая (253 теста + подтесты), -race
make test-trips-db      # тесты хранилища и API поездок на временном PostgreSQL (в make test они пропускаются)
make lint               # go vet + golangci-lint v2.14.0 (бинарник golangci-lint, либо `go run`)
make up && make test-integration
```

Unit-тесты: три клиента upstream на **записанных реальных ответах** (`internal/clients/*/testdata`), хендлеры (валидация, маппинг в DTO, fallback Photon→Nominatim, недоступный Redis, правила кэша, auth, CORS, rate limit, пробы, тайлы, статика, каталог стилей и смена дефолтного стиля с сохранением), пайплайн на скриптованном Docker (каждый шаг, сбой, внедрённый в каждый из 9 шагов, откат, блокировка, очистка, cron), сервер PMTiles на синтетическом архиве вместе с переключением версии, генератор стилей и движок тем, трекер поездок на синтетических треках (сход с маршрута, дублёр, разворот, двор у старта, пауза в фоне, прибытие, темп перестроений), хранилище поездок на настоящем PostgreSQL (миграции, в том числе параллельные, блокировка поездки, повтор батча, снимок для наблюдателя, истечение и очистка, `LISTEN`/`NOTIFY` с переподключением), API поездок на нём же с подменённым OSRM (создание, токены водителя и наблюдателя, батчи позиций и их повтор, неверные часы устройства, перестроение и его повтор при недоступном OSRM, прибытие, завершение и отмена, лимит на поездку, истечение и очистка; поток событий через настоящий HTTP-сервер и `LISTEN`/`NOTIFY`: снимок, события по порядку, досылка по `Last-Event-ID` и снимок вместо слишком длинной досылки, 204 после завершения, heartbeat, лимит потоков, закрытие при остановке; история поездки постранично), а также тесты репозитория (`test/repo`): `.env.example` полон и валиден, все образы закреплены, закоммиченные стили ссылаются только на имеющиеся спрайты и шрифты и не отстают от тем.

Интеграционные тесты (`-tags=integration`, `test/integration`): готовность, геокод известного места, reverse центра, автодополнение, маршрут по каждому профилю (polyline декодируется и сверяется с началом/концом), шаги и GeoJSON, ошибки валидации, аутентификация, тайл z14 в центре (gzip, слои, 304), самодостаточный стиль и каталог стилей (каждый стиль отдаётся, `/tiles/style.json` совпадает с дефолтным). Значения по умолчанию соответствуют Астане; переменные `IT_*` позволяют подстроить их под другой город.

## Другой город и другой профиль

* Другой город: задайте `CITY_NAME`, `CITY_BBOX`, `CITY_CENTER`, `OSM_SOURCE_URL`, `SMOKE_GEOCODE_QUERY`, выполните `make style-assets` (начальная камера и языки зашиты в стиль), `make init-data`, перезапустите. Для интеграционных тестов поправьте `IT_*`.
* Вырезание полигоном вместо прямоугольника: `EXTRACT_MODE=polygon`, `CITY_POLYGON_FILE=deploy/extract/city.geojson` (любой GeoJSON Polygon/MultiPolygon/Feature, который понимает osmium). bbox всё равно используется для фильтрации и валидации.
* Велосипед: `ROUTING_PROFILES=car,foot,bicycle` и `COMPOSE_PROFILES=scheduler,bicycle`. Собственный профиль OSRM: положите `deploy/osrm/<профиль>.lua` (см. `deploy/osrm/README.md`) — он будет использован вместо штатного.

## Известные ограничения

* **Окно переключения.** Переключение — скользящий перезапуск (см. выше), а не blue/green: маршруты могут отвечать ошибкой ≈ 1 с на профиль, а геокод/reverse — ≈ 2–7 с, пока перезапускаются сервисы. Тайлы и автодополнение остаются доступны. Настоящий swap без простоя потребовал бы двух копий Nominatim за общим алиасом.
* **Память `osmium extract`.** При `EXTRACT_STRATEGY=smart` для файла Казахстана 224 МБ потребовалось 3,5 ГиБ, и расход растёт с размером экстракта страны. Для больших стран сначала вырежьте регион или используйте `complete_ways`/`simple`.
* **Диск и RAM при обновлении.** Кандидат Nominatim работает рядом с живым (≈ 0,5–1,5 ГиБ для города такого размера); большим городам нужен запас пропорционально больше, чем намекают цифры простоя.
* **Языки Photon** фиксируются при импорте (`LANGUAGES`); неизвестный `lang` отклоняет gateway, а не Photon (тот ответил бы 400).
* **Nominatim `reverse`** возвращает ближайший объект на уровне здания, и это может быть POI внутри здания (банкомат в ТЦ), а не само здание.
* **Обновления по cron требуют Docker-сокет** (см. безопасность). Updater должен работать внутри сети compose (так по умолчанию), чтобы видеть контейнеры-кандидаты.
* Тайлы строятся без слоёв береговой линии/Natural Earth (им нужны сотни МБ сторонних shapefile, а внутриматериковому городу они не нужны); для приморского города надо вернуть `ocean` в `deploy/tiles/config-openmaptiles.json`.
* Проверено на arm64 (Docker Desktop 4.16 / движок 20.10.22). Образы мультиархитектурные, но прогон на Linux/amd64 не выполнялся.

## Лицензии и атрибуция

Данные © OpenStreetMap contributors (ODbL). Любая карта на этом стеке обязана показывать **«© OpenStreetMap contributors»** и **«© OpenMapTiles»** (стиль это делает). Стили Positron и Dark Matter дополнительно показывают «© CARTO» (их дизайн — CC-BY 3.0). Сторонние ассеты и их лицензии перечислены в [`deploy/tiles/NOTICE.md`](deploy/tiles/NOTICE.md): OSM Liberty (BSD / CC-BY 3.0 / Maki CC0), Positron и Dark Matter (BSD / CC-BY 3.0), Noto Sans (SIL OFL 1.1), MapLibre GL JS (BSD-3), профиль tilemaker (схема CC-BY 4.0). Nominatim — GPL-2.0, Photon — Apache-2.0, OSRM — BSD-2-Clause.
