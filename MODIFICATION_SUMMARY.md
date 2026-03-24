# 数据库与文件账号处理逻辑同步修改总结

## 问题分析
- **文件模式**：账号的运行时状态（Status、CooldownUntil、DisableReason、LastUsedAt）存储在内存中
- **数据库模式**：账号状态未被持久化，导致重新加载时状态被重置，造成"账号切换了之后还是不可用"的问题

## 修改清单

### 1. 数据库 Schema 升级 (`internal/db/schema.go`)
**添加新的状态列到 `codex_accounts` 表：**
- `status` (TINYINT/INTEGER/SMALLINT)：账号状态（0=active, 1=cooldown, 2=disabled）
- `cooldown_until` (DATETIME/TEXT/TIMESTAMPTZ)：冷却结束时间
- `disable_reason` (VARCHAR/TEXT)：禁用原因编码
- `last_used_at` (DATETIME/TEXT/TIMESTAMPTZ)：最后一次成功使用时间

**新增函数：**
- `migrateAddStatusColumns()`：为旧表自动迁移，添加新的状态列
- `addColumnIfNotExists()`：支持三种数据库方言的列添加

### 2. Token 持久化升级 (`internal/auth/manager.go`)
**函数：`saveTokenToDB()`**
- 现在保存完整的运行时状态：`status`、`cooldown_until`、`disable_reason`、`last_used_at`
- 同时保存 Token 信息和账号状态
- 支持 MySQL、SQLite、PostgreSQL 三种方言的 UPSERT 操作

### 3. 账号加载与恢复 (`internal/auth/manager.go`)
**函数修改：**
- `accountFromDBRow()`：
  - 新增参数：`status`, `cooldown_until`, `disableReason`, `lastUsedAt`
  - 恢复账号的运行时状态（Status、CooldownUntil、DisableReason、LastUsedAt）
  - 同步更新原子状态字段（atomicStatus、atomicCooldownMs）

- `loadAccountsFromDB()`：
  - SQL 查询扩展，包含新的状态列
  - 传递状态数据给 `accountFromDBRow()`

- `loadAccountsFromDBSlice()`：
  - 同样支持新的状态列查询和恢复

### 4. 状态变更时的持久化 (`internal/auth/manager.go`)
**函数修改：**
- `applyFinalHTTPRefresh()`：
  - 在 SetCooldown 后调用 `enqueueSave(acc)` 保存冷却状态到数据库
  
- `applyFinalHTTPQuota()`：
  - 在 SetCooldown/SetQuotaCooldown 后调用 `enqueueSave(acc)` 保存状态到数据库

## 工作原理

### 文件模式（未改变）
1. 账号状态存储在内存中
2. 状态变更后通过扫描和重新加载恢复

### 数据库模式（已改进）
1. 账号初始加载时，从数据库恢复运行时状态
2. 账号状态变更（冷却、禁用）时：
   - 修改内存状态
   - 通过 `enqueueSave()` 入队异步持久化
   - SaveWorker 异步保存到数据库
3. 下次加载时，恢复持久化的状态
4. 选号时过滤冷却/禁用的账号

## 关键改进

### 问题解决
- ✅ 账号冷却状态现在被正确持久化到数据库
- ✅ 禁用原因保存，便于调试和统计
- ✅ 最后使用时间保存，用于选号策略优化
- ✅ 重启服务后账号状态被正确恢复

### 兼容性
- ✅ 自动迁移旧数据库，未来会添加新的列
- ✅ 支持三种数据库方言：MySQL、SQLite、PostgreSQL
- ✅ 文件模式逻辑完全保留，无影响

## 测试建议

1. **启用数据库模式** - 使用 MySQL/SQLite/PostgreSQL
2. **观察账号冷却** - 发送请求导致 429/401 错误
3. **查看数据库** - 验证 `status` 和 `cooldown_until` 字段是否更新
4. **重启服务** - 验证账号状态是否被正确恢复
5. **选号行为** - 验证冷却中的账号不被选中

## 相关文件修改
- `/internal/db/schema.go` - 数据库 schema 和迁移
- `/internal/auth/manager.go` - 账号加载、保存、状态恢复

## 注意事项
- 第一次启动时会自动为数据库添加新的列（无需手动 SQL）
- 冷却状态仅在运行时有效，过期自动清除
- 建议在低流量时段升级，以便前景观处完成
