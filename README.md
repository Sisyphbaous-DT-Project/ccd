<p align="center"><img src="docs/logo-512.png" width="240" alt="ccd"></p>

<h1 align="center">ccd</h1>

<p align="center">模型的思维链太长、太啰嗦？ccd 像 Claude 那样把它转述成几句简短的工作笔记再展示给用户。<br>给 <a href="https://github.com/QuantumNous/new-api">new-api</a> 用，模型本身的思考一点不受影响。</p>

<p align="center">中文 | <a href="README.en.md">English</a></p>

---

## 这是什么

推理模型的思维链动辄几千上万字：反复验算、来回推翻、绕圈子。原样流式展示给用户，既看不过来，也抓不住重点。Claude 的做法是 [summarized thinking](https://platform.claude.com/docs/en/build-with-claude/thinking)：不直接展示原始思维链，而是由另一个模型把它转述成简短的摘要。

ccd 把同样的做法带到了 new-api 上。它是一个放在 new-api 前面的小服务：

- 模型照常思考，推理强度、思考预算原样透传。ccd 把原始思维链一段一段交给另一个模型，转述成第一人称、一两句话的「工作笔记」，边写边流式展示给用户。
- **原始思维链不发给客户端**，存在服务器上。客户端下一轮把笔记带回来时，ccd 原样换回原版再交给上游——模型在多轮对话、工具调用里看到的始终是自己的原始思维链。
- 规则在 new-api 后台配置：可以按分组、按模型，或者直接选某一个 key；转述语言可选中文、English 或任意自定义语言。

## 效果展示

<p align="center"><img src="docs/demo-1.png" width="720" alt="效果展示 1"></p>
<p align="center"><img src="docs/demo-2.png" width="720" alt="效果展示 2"></p>
<p align="center"><img src="docs/demo-3.png" width="720" alt="效果展示 3：自定义语言（俄语、日语）"></p>

## 特性

- **只改用户看到的思维链**：正文、工具调用、usage（含 reasoning tokens）、id、model、finish_reason、签名、事件顺序一律不动。
- **原版换回**：原始思维链按用户隔离持久化到磁盘，服务重启不丢；客户端每轮改系统提示词（比如带上当前时间）也能换回；转述失败时也能换回。
- **没命中规则的请求逐字节透传**，连请求体都不解析。
- **格式**：`/v1/chat/completions` 与 Anthropic `/v1/messages`，流式和同步都支持，包括多个 thinking 块穿插工具调用、`redacted_thinking`。
- **后台面板**：注入 new-api 日志页，管理员可以增删改规则、选转述语言和转述模型、一键测试转述。
- **转述用你自己的 key**：转述模型从你已有的 key 里选，用量记在这个 key 上，不会偷偷建令牌。
- **数据库**：只读 new-api 的数据库认 key，支持 SQLite / MySQL / PostgreSQL。
- **没有违禁词、没有预设话术**：转述失败就略过那一段，绝不输出固定句子。

## 工作原理

```
客户端 ──► 反向代理（nginx） ──► ccd ──► new-api ──► 上游模型
                                   │
                                   ├─ 认 key：只读 new-api 数据库（这个 key 属于谁、在哪个分组）
                                   ├─ 回复：思维链交给转述模型（经 new-api，用你选的 key）写成笔记，原版存盘
                                   └─ 下一轮请求：把历史里的笔记换回原版，再交给 new-api
```

ccd 只在两处改字节：回复里的思维链文字（换成笔记），以及请求历史里的思维链文字（换回原版）。用的是带字节偏移的 JSON 局部替换，其余字节——字段顺序、数字写法、转义、空白——一个都不动；没有要改的内容时直接转发原始字节。

## 要求

- new-api v1.0（需要带 `/api/user/auth/refresh` 的版本；在 v1.0.0-rc.43 上测试过）
- new-api 的数据库：SQLite、MySQL 或 PostgreSQL（ccd 只执行 SELECT）
- 从源码构建需要 Go 1.26 或更高

## 快速开始

### 1. 运行 ccd

```bash
git clone https://github.com/Sisyphbaous-DT-Project/ccd.git
cd ccd
go build -o ccd .

CCD_NEWAPI=http://127.0.0.1:3000 \
CCD_SQLITE_PATH=/path/to/new-api/data/one-api.db \
./ccd
```

- 用 MySQL / PostgreSQL 时把 `CCD_SQLITE_PATH` 换成 `CCD_SQL_DSN`，写法与 new-api 的 `SQL_DSN` 相同。
- systemd 示例见 [`deploy/ccd.service`](deploy/ccd.service)。
- Docker：`docker build -t ccd .`，与 new-api 一起跑的示例见 [`deploy/docker-compose.example.yml`](deploy/docker-compose.example.yml)。

> 用 SQLite 时，ccd 要能写 new-api 数据目录里的 `-shm` 文件（SQLite 的 WAL 模式下只读查询也需要），请用和 new-api 相同的系统用户运行，Docker 里挂同一个数据卷。

### 2. 接入反向代理

完整示例见 [`deploy/nginx.conf.example`](deploy/nginx.conf.example)，要点三条：

1. 模型请求（`/v1/`、`/v1beta/`）转给 ccd，upstream 里把 new-api 设为 `backup`：ccd 连不上时自动回落，不影响使用。
2. `/ccd/` 整个转给 ccd（管理接口和面板脚本都在这下面）。
3. 其余请求照常给 new-api，并用 `sub_filter` 把 `<script src="/ccd/panel.js"></script>` 注入页面。

### 3. 配置规则

用管理员账号打开 new-api 的「使用日志」页，工具栏里会多一个「思维链转述」按钮（找不到工具栏时显示在右下角）：

1. **作用对象**：选「分组」（整个分组或勾选模型），或者选「key」（这个 key 发的请求全部套上转述，优先级最高）。
2. **转述语言**：中文 / English / 自定义（直接写「日语」「法语」「Русский」……）。
3. **转述用的 key + 转述模型**：从你已有的 key 里选一个，再选它所在分组里的模型；建议用便宜、快、能关思考的模型。
4. 点「测试转述」看看效果和耗时，再保存。

## 配置项

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `CCD_LISTEN` | `127.0.0.1:3140` | 监听地址 |
| `CCD_NEWAPI` | `http://127.0.0.1:3000` | new-api 地址（管理员校验、读 key 列表、转述调用） |
| `CCD_UPSTREAM` | 同 `CCD_NEWAPI` | 模型请求转给谁；中间还有别的网关时可以改 |
| `CCD_SUMMARY_URL` | `CCD_NEWAPI` + `/v1/chat/completions` | 转述调用的地址 |
| `CCD_SQLITE_PATH` | — | new-api 的 SQLite 文件 |
| `CCD_SQL_DSN` | — | new-api 的 MySQL / PostgreSQL 连接串，写法同 new-api 的 `SQL_DSN` |
| `CCD_STATE` | `/var/lib/ccd` | 规则和原版思维链的存放目录 |
| `CCD_STORE_TTL_DAYS` | `30` | 原版思维链保留天数 |
| `CCD_STORE_MAX_MB` | `512` | 原版思维链总容量上限，超了按最旧淘汰 |

每条规则的高级设置（面板里默认收起）：流式 / 同步切段参数（默认 100/200/1800、250/150/1800 字）、每个请求的转述并发（默认 3 / 6）、单段超时（默认 20 秒）、正文最多等摘要多久（默认一直等）、关思考参数（默认 `{"reasoning_effort":"off","thinking":{"type":"disabled"}}`，只用在转述调用上，不会碰用户的请求）。

## 安全

- ccd 对 new-api 数据库只执行 SELECT。
- 管理接口每次都拿登录态调 new-api 的 `/api/user/self` 校验，`role >= 100` 才放行。
- key 列表只下发 id、名字、分组、状态，key 本身不会发到前端；转述用的 key 只能是管理员自己名下、启用中、未过期的，随用随取，不落盘。
- 原始思维链按用户分目录存放，目录权限 0700、文件 0600。

## 已知限制

- `/v1/responses`、Gemini 原生接口、embeddings / rerank 目前原样透传，不转述。
- 转述由另一次模型调用完成，用户看到第一句笔记会比上游开始思考晚一点（摘要的固有成本）；面板里可以设置「正文最多等摘要多久」。
- 客户端如果不把思维链带回来，就没有可换回的内容（这和不用 ccd 时一样）。
- 绕过反向代理直连 new-api 的请求不经过 ccd，不会被转述。

## 开发

```bash
go vet ./...
go test -race ./...
```

`TestNewAPIDirectoryOnRealDatabases` 会在设置了 `CCD_TEST_MYSQL_DSN` / `CCD_TEST_PG_DSN` 时，在真实的 MySQL / PostgreSQL 上跑一遍查询（会建、删表，请用空的测试库）。GitHub Actions 每次提交都会自动跑。

## 参考

- Anthropic：[Thinking](https://platform.claude.com/docs/en/build-with-claude/thinking)（summarized thinking、signature、progress updates）
- [new-api](https://github.com/QuantumNous/new-api)

## 许可证

[AGPL-3.0](LICENSE)。Copyright (C) 2026 C₂₂H₂₅NO₆
