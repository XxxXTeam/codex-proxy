# Changelog

## [1.7.0](https://github.com/XxxXTeam/codex-proxy/compare/v1.6.2...v1.7.0) (2026-04-22)


### ✨ 新功能

* 实验性支持Image模型，支持命令行OAuth登录授权Codex账号 ([40a9bfd](https://github.com/XxxXTeam/codex-proxy/commit/40a9bfd722b254fea8e90000eeb759aaaf932fff))


### 📦 依赖更新

* **go:** bump modernc.org/sqlite from 1.48.2 to 1.49.1 ([#33](https://github.com/XxxXTeam/codex-proxy/issues/33)) ([dffa40f](https://github.com/XxxXTeam/codex-proxy/commit/dffa40f6f075afea410bc3e919b0254847687f51))

## [1.6.2](https://github.com/XxxXTeam/codex-proxy/compare/v1.6.1...v1.6.2) (2026-04-16)


### 🐛 错误修复

* 修复1m上下文与fast模式参数传递错误问题，细节优化 ([d601552](https://github.com/XxxXTeam/codex-proxy/commit/d60155275d1ef2d68eb6b26e2e1b2b239a23cd73))

## [1.6.1](https://github.com/XxxXTeam/codex-proxy/compare/v1.6.0...v1.6.1) (2026-04-14)


### 📦 依赖更新

* **ci:** bump softprops/action-gh-release from 2 to 3 ([#30](https://github.com/XxxXTeam/codex-proxy/issues/30)) ([b5d0bd1](https://github.com/XxxXTeam/codex-proxy/commit/b5d0bd19f52acac22a28a4deea1289643232019a))
* **go:** bump github.com/valyala/fasthttp from 1.69.0 to 1.70.0 ([#29](https://github.com/XxxXTeam/codex-proxy/issues/29)) ([abadc64](https://github.com/XxxXTeam/codex-proxy/commit/abadc64a705bdf6562063f4ea5bf33ae76264890))
* **go:** bump golang.org/x/net in the golang-org-x group ([#27](https://github.com/XxxXTeam/codex-proxy/issues/27)) ([90bf8e7](https://github.com/XxxXTeam/codex-proxy/commit/90bf8e71dbfefad47229e86b3040159904f6a406))
* **go:** bump modernc.org/sqlite from 1.48.1 to 1.48.2 ([#28](https://github.com/XxxXTeam/codex-proxy/issues/28)) ([2956c27](https://github.com/XxxXTeam/codex-proxy/commit/2956c273e1f8109e830ef884b9a88a55181f885c))


### 🎡 持续集成

* 更新依赖版本以及go.sum文件 ([1507119](https://github.com/XxxXTeam/codex-proxy/commit/15071197f2d1b93c182920a109284fe69b026f40))

## [1.6.0](https://github.com/XxxXTeam/codex-proxy/compare/v1.5.0...v1.6.0) (2026-04-11)


### ✨ 新功能

* 修复部分情况下的账号请求429问题，优化换号与请求实现，完善配置文件相关示例 ([0a59ad9](https://github.com/XxxXTeam/codex-proxy/commit/0a59ad9de8257c854a42a868e4d094920a07b9c4))
* 添加可选是否显示1m and fast ([53fddea](https://github.com/XxxXTeam/codex-proxy/commit/53fddeabf7504fda2ad21d72a1e377981af7dcd0))


### 📦 依赖更新

* **ci:** bump actions/download-artifact from 4 to 8 ([#23](https://github.com/XxxXTeam/codex-proxy/issues/23)) ([37ed431](https://github.com/XxxXTeam/codex-proxy/commit/37ed43151b998b035991412f681c1d5b88b2839a))
* **ci:** bump actions/upload-artifact from 4 to 7 ([#24](https://github.com/XxxXTeam/codex-proxy/issues/24)) ([eda64e7](https://github.com/XxxXTeam/codex-proxy/commit/eda64e7c14f277700c892715bb24eea796498c74))
* **go:** bump github.com/lib/pq from 1.12.0 to 1.12.3 ([#22](https://github.com/XxxXTeam/codex-proxy/issues/22)) ([25e876e](https://github.com/XxxXTeam/codex-proxy/commit/25e876e30f94a63d79d5a97bc089296130dd6850))
* **go:** bump modernc.org/sqlite from 1.48.0 to 1.48.1 ([#21](https://github.com/XxxXTeam/codex-proxy/issues/21)) ([a86e8ef](https://github.com/XxxXTeam/codex-proxy/commit/a86e8eff0eebf5a50e9fbe68dd19997f983f31c2))

## [1.5.0](https://github.com/XxxXTeam/codex-proxy/compare/v1.4.0...v1.5.0) (2026-04-02)


### ✨ 新功能

* **proxy:** 添加 /v1/responses/compact 路由和测试 ([e997591](https://github.com/XxxXTeam/codex-proxy/commit/e997591e76cd28ace393394964c2f6c7ae42864f))


### 🐛 错误修复

* 删除测试文件 ([bea00da](https://github.com/XxxXTeam/codex-proxy/commit/bea00da3a567691a8313487901350bab084ae3c7))

## [1.4.0](https://github.com/XxxXTeam/codex-proxy/compare/v1.3.0...v1.4.0) (2026-03-31)


### ✨ 新功能

* 支持自适应连接池配置，优化请求与转发性能 ([b73d9a1](https://github.com/XxxXTeam/codex-proxy/commit/b73d9a10b3a5325bbed85df4630ac5ee906f3339))


### 🐛 错误修复

* 优化工作流打包文件错误问题 ([2381ddb](https://github.com/XxxXTeam/codex-proxy/commit/2381ddb4deec2405e82a6b94c3339ada23954b3f))
* 修复工作流配置权限错误问题 ([1ed95c9](https://github.com/XxxXTeam/codex-proxy/commit/1ed95c9459cb751b079c7b5ac3feccefd8f800d4))

## [1.3.0](https://github.com/XxxXTeam/codex-proxy/compare/v1.2.1...v1.3.0) (2026-03-31)


### ✨ 新功能

* 适配1m模型，修复fast模型参数传递问题，性能优化 ([3906118](https://github.com/XxxXTeam/codex-proxy/commit/3906118993315ee813f919e96c14b40dfc5fe3ac))

## [1.2.1](https://github.com/XxxXTeam/codex-proxy/compare/v1.2.0...v1.2.1) (2026-03-30)


### 🐛 错误修复

* 修复在auth文件为空时的panic问题，支持rk为空或null的支持 ([4214ee9](https://github.com/XxxXTeam/codex-proxy/commit/4214ee9b6d30657cdb26d84ae5c4d75d433dbfbe))


### 📦 依赖更新

* **ci:** bump actions/checkout from 4 to 6 ([#14](https://github.com/XxxXTeam/codex-proxy/issues/14)) ([ed47be0](https://github.com/XxxXTeam/codex-proxy/commit/ed47be0f38666a716f7a83973748886b0303a7a2))
* **ci:** bump actions/setup-go from 5 to 6 ([#15](https://github.com/XxxXTeam/codex-proxy/issues/15)) ([3dda587](https://github.com/XxxXTeam/codex-proxy/commit/3dda5878976d19aa7b9f443f33909773cdae21e1))
* **go:** bump modernc.org/sqlite from 1.47.0 to 1.48.0 ([#13](https://github.com/XxxXTeam/codex-proxy/issues/13)) ([955d498](https://github.com/XxxXTeam/codex-proxy/commit/955d4987eac279f08ac0002fdc40c59791d34acd))

## [1.2.0](https://github.com/XxxXTeam/codex-proxy/compare/v1.1.5...v1.2.0) (2026-03-27)


### ✨ 新功能

* pump-phase upstream retry, ExecuteNonStream scanner error handling, and openCodexResponsesBody struct return ([#10](https://github.com/XxxXTeam/codex-proxy/issues/10)) ([7bed650](https://github.com/XxxXTeam/codex-proxy/commit/7bed650a919d98d952125b0bf3dea5712390c0ae))
* retry on upstream error during stream pump; fix ExecuteNonStream scanner error handling ([93d5560](https://github.com/XxxXTeam/codex-proxy/commit/93d55602eb72c3d7be3d725962c76510d2c21939))


### 🐛 错误修复

* 修复部分情况没有走代理的bug,添加代理启动时检查 ([c1322e2](https://github.com/XxxXTeam/codex-proxy/commit/c1322e2517654ed669e5d6d5240831d4536e77f9))


### ♻️ 代码重构

* group openCodexResponsesBody returns into struct and remove unused context params ([c19603f](https://github.com/XxxXTeam/codex-proxy/commit/c19603fcc2c90156e49f1986d9b12bdc84b6f724))


### 📦 依赖更新

* **go:** bump filippo.io/edwards25519 from 1.1.0 to 1.1.1 ([#8](https://github.com/XxxXTeam/codex-proxy/issues/8)) ([b3552db](https://github.com/XxxXTeam/codex-proxy/commit/b3552db88f9d5d5ec5668c500197c63e78d6b41c))


### 🎡 持续集成

* 优化工作流 ([c13a796](https://github.com/XxxXTeam/codex-proxy/commit/c13a796aaa344d6a590d7cbd5805991a3a2132b4))
* 修复工作流 ([f5b4e6b](https://github.com/XxxXTeam/codex-proxy/commit/f5b4e6b562d11ce33729350b81c93fe3e824a6a5))
* 修复自动发版 ([524dd12](https://github.com/XxxXTeam/codex-proxy/commit/524dd12858dbcc0eb32ce3f6bbd571f7ee780745))
* 分支写错了... ([88e19dc](https://github.com/XxxXTeam/codex-proxy/commit/88e19dc33137ba4f88aa158b9912664645f26424))
* 添加自动发版 ([815b727](https://github.com/XxxXTeam/codex-proxy/commit/815b7271b09c4b8cfdae4a593b3635a7862ffde3))
