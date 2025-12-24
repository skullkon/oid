# OIDC пример на Go + краткий ресерч

Этот репозиторий содержит **минимальный пример OIDC (OpenID Connect)** на Go и краткий ресерч, как жить с OIDC рядом с вашим текущим SAML/AD FS флоу без изменения фронтенда.

## Что внутри

- `main.go` — простой HTTP‑сервер на Go с OIDC логином (Authorization Code Flow + опциональный PKCE).
- `go.mod` — зависимости (`go-oidc`, `oauth2`).

## Быстрый обзор: что такое OIDC

OIDC — слой аутентификации поверх OAuth 2.0.

- **ID Token** (JWT) — подтверждает личность пользователя и содержит claims.
- **Access Token** — используется для доступа к API.
- **Refresh Token** — обновляет access token.

Типовой поток:
1. Пользователь редиректится на IdP (authorization endpoint).
2. IdP аутентифицирует пользователя.
3. IdP возвращает `code` на `redirect_uri`.
4. Ваш сервис обменивает `code` → токены.
5. Проверяет `ID Token` и выдаёт сессию.

## Как у вас сейчас (по описанию)

- Пользователь вводит логин/пароль от корпоративного SSO (SAML через AD FS/Directory).
- IdP присылает assertion с группами (AD groups), которые «навешаны» на сервис.
- Сервис по этим группам мапит привилегии (БД ролей/пермов).
- Фронт про SAML не знает — он просто зовёт единый login‑URL.

## Как прикрутить OIDC без Keycloak/шлюзов, оставив фронт как есть

**Идея:** использовать ваш AD FS (или Azure AD) как OpenID Provider напрямую. AD FS умеет OAuth2/OIDC, так что можно не ставить внешние «шлюзы».

1) **Зарегистрировать OIDC Client** в AD FS (Application Group, Web API + Web App).  
2) **Включить scopes/claims** с группами:  
   - Добавить выдачу `roles` или `groups` claim (правило трансформации групп).  
   - При необходимости сужать список групп до тех, что относятся к сервису.  
3) **Настроить redirect_uri** = `http://localhost:8080/callback` (или ваш реальный).  
4) **В сервисе** использовать OIDC discovery `https://<adfs>/adfs/.well-known/openid-configuration` как `OIDC_ISSUER`.  
5) **Фронт** по-прежнему открывает `/login`. Бэкенд редиректит на AD FS OIDC вместо SAML. Пользователь видит тот же корпоративный логин.

Маппинг привилегий остаётся тем же: группы из токена → ваши роли в БД. Отличие в том, что вместо SAML assertion прилетает `id_token` (JWT) с claim’ами.

## Можно ли использовать OIDC рядом с SAML, чтобы фронт ничего не менял?

**Да, можно**, но обычно это решают **на стороне IdP**, чтобы фронт не трогать:

### Вариант A (для смешанного периода): IdP как шлюз

- Ваш фронт остаётся как есть: редиректит пользователя на один URL логина.
- **IdP принимает SAML** (от старого провайдера) и **выдаёт OIDC** вашему сервису.
- В итоге ваш сервис работает только по OIDC, а SAML скрыт внутри IdP.

Так умеют Keycloak, Auth0, Azure AD, Okta и другие. Для чистого AD FS можно обойтись без шлюза, см. раздел выше.

### Вариант B: сервис поддерживает оба протокола

- Ваш backend/edge принимает и SAML, и OIDC.
- Фронту можно оставить один URL логина и выбирать протокол на бэке.
- Минус: больше кода, сложнее сопровождение.

**Итог:** чтобы фронт не трогать — лучше ставить IdP‑шлюз (федерацию SAML→OIDC).

## MVP‑сценарий (минимально жизнеспособный)

1. Поднять IdP (например, Keycloak).
2. Создать OIDC Client.
3. Настроить `redirect_uri` на сервис.
4. Реализовать login/callback в сервисе (пример в `main.go`).
5. Выдавать роли/claims через IdP **или** хранить их в сервисе.

## Запуск примера

### 1) Подготовить IdP (пример: Keycloak)

В Keycloak:
- Создать Realm.
- Создать Client (OIDC).
- Указать `redirect_uri` = `http://localhost:8080/callback`.
- Включить `Standard Flow`.

### 2) Установить переменные окружения

```bash
export OIDC_ISSUER="http://localhost:8081/realms/demo"
export OIDC_CLIENT_ID="demo-client"
export OIDC_CLIENT_SECRET="your-secret"  # если confidential client
export OIDC_REDIRECT_URL="http://localhost:8080/callback"
export OIDC_SCOPES="openid,profile,email"
export OIDC_USE_PKCE="true"
```

### 3) Запуск

```bash
go run .
```

### 4) Проверка

- `http://localhost:8080/login` — login
- `http://localhost:8080/me` — JSON с claims
- `http://localhost:8080/logout` — logout

## Где хранить роли / привилегии

- **В IdP** (roles / groups) — claims возвращаются в токене.
- **В приложении** — ваш сервис хранит роли в БД и выдаёт права на основании `sub`/`email`.
- **Смешанно** — глобальные роли в IdP, локальные права внутри сервиса.

## Переменные окружения

| Переменная | Обязательная | Назначение |
|---|---|---|
| `OIDC_ISSUER` | ✅ | URL Issuer (realm endpoint) |
| `OIDC_CLIENT_ID` | ✅ | ID клиента |
| `OIDC_CLIENT_SECRET` | ❌ | Secret (если confidential client) |
| `OIDC_REDIRECT_URL` | ✅ | Redirect URL для callback |
| `OIDC_SCOPES` | ❌ | Скоупы, по умолчанию `openid,profile,email` |
| `OIDC_USE_PKCE` | ❌ | `true` для PKCE |
| `SESSION_COOKIE` | ❌ | Название cookie |
| `LISTEN_ADDR` | ❌ | Адрес для сервера (`:8080`) |

## Ограничения примера

- Сессии хранятся **в памяти**.
- Нет шифрования/подписывания cookie.
- Это демо для понимания флоу. Для продакшена нужен устойчивый session storage и защита.

## Как бы выглядела интеграция в проде

- Вынести сессии в Redis/Postgres.
- Использовать HTTPS, secure cookies.
- Добавить аудит и логи входов.
- Включить обновление токенов и ротацию refresh tokens.
- Привязать роли и разрешения к бизнес‑логике.
