# Changelog

## [1.2.1](https://github.com/XxxXTeam/codex-proxy/compare/v1.2.0...v1.2.1) (2026-03-30)


### 🐛 错误修复

* 修复在auth文件为空时的panic问题，支持rk为空或null的支持 ([4214ee9](https://github.com/XxxXTeam/codex-proxy/commit/4214ee9b6d30657cdb26d84ae5c4d75d433dbfbe))

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
