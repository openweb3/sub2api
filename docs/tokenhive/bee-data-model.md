# Bee 数据模型设计

## 1. 设计范围

本文定义 TokenHive 第一阶段中 Bee 及其平台共享能力的数据模型。

第一阶段只支持单 Hive：

- Bee 的基本信息和平台绑定关系持久化到 PostgreSQL
- Bee 的实时连接和在线状态保存在 Hive 进程内
- Hive 重启后，Bee App 需要重新建立 WebSocket 连接
- 暂不处理多 Hive 之间的连接归属和消息路由

Bee App 使用当前项目已有的 User 体系登录，不建立独立的 Bee 用户体系。

## 2. 领域关系

```text
User 1 ── N Bee 1 ── N BeePlatform
```

- `User`：当前项目已有的用户，也是 Bee Owner
- `Bee`：一个已注册的 Bee App 安装实例
- `BeePlatform`：Bee 在某个平台上的共享能力与平台身份

约束：

- 一个 User 可以拥有多个 Bee
- 一个 Bee 只能属于一个 User
- 一个 Bee 可以支持多个平台
- 同一个 Bee 对同一个平台最多存在一条有效的 BeePlatform 记录
- 同一个平台账号只能绑定到一个 Bee

## 3. bee 表

`bee` 表表示一个已注册的 Bee App 安装实例。

| 字段 | 建议类型 | 必填 | 含义 |
|---|---|---:|---|
| `id` | `BIGINT` | 是 | Bee 内部主键 |
| `user_id` | `BIGINT` | 是 | Bee Owner，关联现有 `users.id` |
| `device_id` | `UUID` | 是 | Bee App 安装实例生成的稳定标识 |
| `name` | `VARCHAR(100)` | 是 | 用户可识别的设备名称 |
| `status` | `VARCHAR(20)` | 是 | 管理状态：`active`、`disabled`、`revoked` |
| `credential_hash` | `VARCHAR` | 是 | Bee WebSocket Credential 的哈希 |
| `credential_created_at` | `TIMESTAMPTZ` | 是 | 当前 Bee Credential 的签发或轮换时间 |
| `app_version` | `VARCHAR(50)` | 否 | 最近连接时上报的 Bee App 版本 |
| `last_connected_at` | `TIMESTAMPTZ` | 否 | 最近一次成功建立 WebSocket 的时间 |
| `last_disconnected_at` | `TIMESTAMPTZ` | 否 | 最近一次连接断开的时间 |
| `last_seen_at` | `TIMESTAMPTZ` | 否 | 最近一次确认 Bee 存活的时间 |
| `created_at` | `TIMESTAMPTZ` | 是 | 创建时间 |
| `updated_at` | `TIMESTAMPTZ` | 是 | 更新时间 |
| `deleted_at` | `TIMESTAMPTZ` | 否 | 软删除时间 |

建议约束：

```text
PRIMARY KEY (id)
FOREIGN KEY (user_id) REFERENCES users(id)
UNIQUE (device_id)
```

建议索引：

```text
INDEX (user_id)
INDEX (status)
INDEX (deleted_at)
```

### 3.1 device_id

`device_id` 标识 Bee App 安装实例，不是物理硬件 ID，也不是认证凭证。

建议由 Bee App 首次启动时生成随机 UUID，并保存在本地：

```text
首次安装：生成新的 device_id
App 重启：继续使用原 device_id
重新安装：生成新的 device_id，视为一个新 Bee
```

不使用 MAC 地址、硬盘序列号等硬件指纹。

### 3.2 Bee Credential

User Access Token 用于注册和管理 Bee；Bee Credential 用于 Bee WebSocket 连接和任务通信。

Hive 只保存 Bee Credential 的哈希，明文仅在注册或轮换时返回一次。Bee App 应将明文凭证保存在操作系统安全存储中。

第一阶段每个 Bee 只保留一个有效 Credential，因此可以将 `credential_hash` 直接放在 `bee` 表。以后需要多凭证、轮换历史或独立撤销记录时，再拆分 `bee_credentials` 表。

## 4. bee_platform 表

`bee_platform` 表表示 Bee 在一个具体平台上的共享能力和平台身份。

| 字段 | 建议类型 | 必填 | 含义 |
|---|---|---:|---|
| `id` | `BIGINT` | 是 | BeePlatform 内部主键 |
| `bee_id` | `BIGINT` | 是 | 所属 Bee |
| `platform` | `VARCHAR(50)` | 是 | `openai`、`anthropic`、`gemini`、`grok` |
| `upstream_account_key` | `VARCHAR(64)` | 是 | 平台额度归属身份的稳定哈希 |
| `identity_version` | `SMALLINT` | 是 | `upstream_account_key` 生成规则版本，默认 `1` |
| `subscription_tier` | `VARCHAR(50)` | 否 | `plus`、`pro`、`team`、`max` 等套餐类型 |
| `concurrency` | `INT` | 是 | Hive 允许该 BeePlatform 同时执行的最大任务数 |
| `quota_snapshot` | `JSONB` | 是 | 最近一次额度信息快照，默认 `{}` |
| `quota_updated_at` | `TIMESTAMPTZ` | 否 | 最近一次更新额度快照的时间 |
| `last_task_at` | `TIMESTAMPTZ` | 否 | 最近一次在该平台执行任务的时间 |
| `status` | `VARCHAR(20)` | 是 | 平台共享状态：`active`、`disabled` |
| `extra` | `JSONB` | 是 | 其他平台特定扩展数据，默认 `{}` |
| `created_at` | `TIMESTAMPTZ` | 是 | 创建时间 |
| `updated_at` | `TIMESTAMPTZ` | 是 | 更新时间 |
| `deleted_at` | `TIMESTAMPTZ` | 否 | 软删除时间 |

建议约束：

```text
PRIMARY KEY (id)
FOREIGN KEY (bee_id) REFERENCES bee(id)
CHECK (platform IN ('openai', 'anthropic', 'gemini', 'grok'))
CHECK (concurrency > 0)
```

建议索引：

```text
INDEX (bee_id)
INDEX (platform)
INDEX (status)
INDEX (deleted_at)
```

### 4.1 平台唯一约束

一个 Bee 对同一个平台最多存在一条未删除记录：

```sql
CREATE UNIQUE INDEX ...
ON bee_platform (bee_id, platform)
WHERE deleted_at IS NULL;
```

同一个平台账号只能绑定到一个 Bee：

```sql
CREATE UNIQUE INDEX ...
ON bee_platform (platform, upstream_account_key)
WHERE deleted_at IS NULL;
```

例如：

```text
Bee A + OpenAI Account X      允许
Bee A + Anthropic Account Y   允许
Bee A + OpenAI Account Z      拒绝：Bee A 已经存在 OpenAI

Bee B + OpenAI Account X      拒绝：Account X 已绑定 Bee A
Bee B + OpenAI Account Z      允许
```

第一阶段采用持久化独占绑定，而不只是限制同时在线：

- Bee 离线后，平台账号仍然绑定在原 Bee 上
- 其他 Bee 不能自动抢占该平台账号
- 迁移平台账号时，必须先显式解绑原 BeePlatform
- 解绑通过软删除原 BeePlatform 记录完成

### 4.2 upstream_account_key

`upstream_account_key` 用于识别同一个平台账号，不能使用会发生轮换的 OAuth access token 或 refresh token。

Bee App 应从平台凭据或平台账号信息中提取稳定的额度归属 ID，并计算：

```text
SHA256(
  "tokenhive:v1:"
  + platform
  + ":"
  + stable_upstream_account_id
)
```

不同平台应分别定义稳定身份提取规则。应选择真正代表额度归属的账号、用户或组织 ID，而不是套餐名称或登录邮箱。

`identity_version` 用于标识身份提取和哈希规则版本，便于以后兼容或迁移。

第一阶段由官方 Bee App 计算并上报 `upstream_account_key`。这个机制可以防止正常客户端重复绑定，但无法独立防止恶意修改的客户端伪造身份指纹。

### 4.3 subscription_tier

不同平台的套餐名称不同，因此 `subscription_tier` 使用受长度限制的字符串，不使用数据库级全局枚举。

例如：

```text
OpenAI: plus / pro / team
Anthropic: pro / max / team
```

业务层可以根据 `platform` 校验已知值，同时允许以后增加新套餐。

### 4.4 concurrency

`bee_platform.concurrency` 是 Hive 为该 BeePlatform 配置的硬并发上限。

Bee 在线时还可以上报当前可用并发，实际有效并发为：

```text
effective_concurrency =
  min(bee_platform.concurrency, Bee 当前上报的 concurrency)
```

第一阶段各平台独立计算并发，不设置 Bee 级总并发上限。

### 4.5 quota_snapshot

`quota_snapshot` 保存最近一次平台额度快照，例如：

```json
{
  "remaining_percent": 72.5,
  "window": {
    "used": 27500,
    "limit": 100000,
    "reset_at": "2026-08-01T00:00:00Z"
  },
  "raw": {}
}
```

`quota_updated_at` 表示该快照的采集时间，用于判断数据是否过期。

Bee 在线时，调度应优先使用当前连接上报的实时额度；数据库快照主要用于离线展示、诊断和最近状态记录。

`extra` 和 `quota_snapshot` 均不得保存：

- OAuth access token
- OAuth refresh token
- Consumer API Key
- Bee Credential 明文

上游平台凭据只保存在 Bee App 本地。

## 5. 在线状态

`bee` 表不保存 `is_online` 字段。

在线状态由单 Hive 的进程内 Bee Connection 注册表动态计算：

```text
online =
  当前存在有效 Bee Connection
  AND 最后心跳未超时
```

数据库中的字段含义：

- `status`：持久化的管理状态
- `last_connected_at`：最近一次连接时间
- `last_disconnected_at`：最近一次断开时间
- `last_seen_at`：最近一次在线证据

`last_seen_at` 不能直接代表当前在线。为避免每次心跳都写数据库，可按固定周期节流更新。

## 6. Bee App 登录与注册

Bee App 使用当前项目现有的 User 登录接口，获得 User Access Token 和 Refresh Token。

建议流程：

```text
1. Bee App 使用现有 User 登录
2. Hive 返回 User Access Token 和 Refresh Token
3. Bee App 使用 User Access Token 注册 device_id
4. Hive 创建或返回 Bee
5. Hive 签发独立 Bee Credential
6. Bee App 使用 Bee Credential 建立 WebSocket
```

注册 `device_id` 时：

```text
device_id 不存在：
  创建 Bee，绑定当前 user_id

device_id 已存在且属于当前 User：
  返回原 Bee，可按策略轮换 Bee Credential

device_id 已存在但属于其他 User：
  拒绝注册，不自动转移所有权

Bee 已 disabled 或 revoked：
  拒绝连接，要求显式恢复
```

User Access Token 用于注册、查看和管理 Bee；Bee Credential 只允许：

- 建立 Bee WebSocket
- 上报自己的 BeePlatform
- 上报心跳和运行状态
- 接收任务并回传结果

Bee Credential 不允许访问用户余额、API Key、其他 Bee 或管理后台。

## 7. 状态影响

- User 被禁用或删除：其所有 Bee 都不能连接
- Bee 被禁用或撤销：只影响该 Bee
- BeePlatform 被禁用：只影响该平台，Bee 的其他平台可以继续工作
- User Access Token 过期：不影响已经合法注册并运行的 Bee
- Bee App 显式退出：断开 WebSocket，并按产品策略删除或撤销本地 Bee Credential
- 网页端普通退出：不必影响后台运行的 Bee

## 8. 第一阶段明确不包含

- 多 Hive 连接归属和跨 Hive 消息路由
- Bee 级总并发上限
- 平台账号自动抢占
- 多 Bee Credential 和凭证历史
- 上游 OAuth 凭据在 Hive 中存储
- 通过数据库 `is_online` 字段维护在线状态
