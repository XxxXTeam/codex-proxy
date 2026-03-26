# 贡献指南

感谢你愿意为 **Codex Proxy** 做出贡献。本文说明协作方式、代码期望与提交流程。

## 行为准则

请保持尊重、专业与建设性沟通。讨论围绕问题与方案本身，避免人身攻击与无关争论。

## 如何参与

- **报告问题**：在仓库 Issues 中选择 **Bug 报告** 模板，尽量提供复现步骤、配置（去掉密钥）、日志与版本信息。
- **功能建议**：在 Issues 中选择 **功能建议** 模板，说明使用场景与期望行为。
- **提交代码**：先 Fork 仓库，在新分支上开发，通过 Pull Request 合并（见下文）。

## 开发环境

- Go **1.25+**
- 克隆后可在仓库根目录执行：

```bash
go build .
./codex-proxy 
```

本地配置可从 `config.example.yaml` 复制为 `config.yaml`，并仅在本地 `auths/` 中使用测试账号。

## 代码与风格

- 与现有代码保持一致：包结构、命名、日志（logrus）、错误处理方式。
- 变更应聚焦单一目的，避免无关格式化或大范围重排。
- 若修改配置项，请同步更新 [`config.example.yaml`](config.example.yaml) 与 [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)（如有行为或默认值变化）。
- 对外行为或部署方式有变时，酌情更新 [`README.md`](README.md) 或 [`docs/DEPLOY.md`](docs/DEPLOY.md)。

## 提交规范（Commit Message）

建议采用 **[Conventional Commits](https://www.conventionalcommits.org/)** 风格，便于生成变更日志与审阅历史。

### 格式

```
<type>(<scope>): <简短描述>

[可选正文：说明动机、实现要点、Breaking changes 等]
```

- **type（必填）**：本次改动的性质，常用取值如下。
- **scope（可选）**：影响范围，如 `config`、`auth`、`handler`、`docker`、`docs`。
- **简短描述（必填）**：用祈使语气，**不要**以句号结尾；中文或英文均可，与历史提交保持一致即可。

### type 取值

| type | 含义 |
|------|------|
| `feat` | 新功能 |
| `fix` | Bug 修复 |
| `docs` | 仅文档 / 注释 |
| `style` | 格式、缺分号等（不改变代码逻辑） |
| `refactor` | 重构（非新功能、非修 bug） |
| `perf` | 性能优化 |
| `test` | 测试相关 |
| `build` | 构建系统或依赖（如 `go.mod`、Dockerfile） |
| `ci` | CI 配置（如 GitHub Actions） |
| `chore` | 其他杂项（工具脚本、无关业务的配置） |

### 约定

1. **原子提交**：一次提交尽量只表达一个完整意图；大改动可拆成多个提交。
2. **第一行长度**：建议不超过 **72** 字符，在常见终端与 GitHub 上可读性更好。
3. **关联 Issue**：可在正文末行写 `Closes #123` 或 `Refs #456`（按需）。
4. **破坏性变更**：在正文或类型后注明，例如正文第一行写 `BREAKING CHANGE: ...`，或使用 `feat!:` / `fix!:`。

### 示例

```
fix(handler): 流式响应在换号重试时重复写入 SSE 头

Refs #42
```

```
docs: 补充 CONFIGURATION 中 db 连接池默认值说明
```

```
feat(config): 支持通过环境变量覆盖 listen 地址
```

若团队更习惯纯中文短句而不加 `type(scope):`，至少保证**第一行能单独说清「做了什么」**，并在 PR 描述中归类（feat/fix/docs 等）。

## Pull Request 流程

1. 从 `main`（或默认分支）创建分支：`feature/xxx` 或 `fix/xxx`。
2. 提交信息请遵循上文 **提交规范**；一个 PR 内提交历史应可读、可逐条 review。
3. 打开 PR 时填写模板中的各项，便于审查。
4. 等待维护者 Review；如需补充测试说明或文档，在 PR 中跟进即可。

若你的改动较大，建议先在 Issue 中简要沟通设计，避免与维护者方向冲突。

## 安全漏洞

请勿在公开 Issue 中张贴可利用的细节与真实密钥。请通过仓库 Security 页面「Private vulnerability reporting」或维护者提供的私密渠道联系。

## 许可证

向本仓库贡献的内容将遵循与项目相同的开源许可证（见 [`LICENSE`](LICENSE)）。提交 PR 即表示你同意在相同许可证下发布你的贡献。
