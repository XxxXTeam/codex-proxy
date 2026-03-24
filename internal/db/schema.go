/**
 * 按方言创建 codex_accounts；去掉未参与查询的二级索引以降低 upsert 写放大；
 * PostgreSQL 使用 fillfactor 缓解高频 UPDATE 的页分裂。
 */
package db

import (
	"database/sql"
	"fmt"
	"strings"
)

func SetupSchema(db *sql.DB, d Dialect) error {
	if db == nil {
		return nil
	}
	var ddl string
	switch d {
	case DialectMySQL:
		ddl = `
CREATE TABLE IF NOT EXISTS codex_accounts (
	id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
	account_id VARCHAR(768) NULL,
	email VARCHAR(768) NULL,
	id_token MEDIUMTEXT,
	access_token MEDIUMTEXT,
	refresh_token MEDIUMTEXT,
	expire VARCHAR(128),
	plan_type VARCHAR(128),
	last_refresh DATETIME(6) NULL,
	status TINYINT DEFAULT 0,
	cooldown_until DATETIME(6) NULL,
	disable_reason VARCHAR(128),
	last_used_at DATETIME(6) NULL,
	updated_at DATETIME(6) NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
	UNIQUE KEY uk_codex_accounts_account_id (account_id),
	UNIQUE KEY uk_codex_accounts_email (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	case DialectSQLite:
		ddl = `
CREATE TABLE IF NOT EXISTS codex_accounts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id TEXT UNIQUE,
	email TEXT UNIQUE,
	id_token TEXT,
	access_token TEXT,
	refresh_token TEXT,
	expire TEXT,
	plan_type TEXT,
	last_refresh TEXT,
	status INTEGER DEFAULT 0,
	cooldown_until TEXT,
	disable_reason TEXT,
	last_used_at TEXT,
	updated_at TEXT DEFAULT CURRENT_TIMESTAMP
)`
	default:
		ddl = `
CREATE TABLE IF NOT EXISTS codex_accounts (
	id SERIAL PRIMARY KEY,
	account_id TEXT UNIQUE,
	email TEXT UNIQUE,
	id_token TEXT,
	access_token TEXT,
	refresh_token TEXT,
	expire TEXT,
	plan_type TEXT,
	last_refresh TIMESTAMPTZ,
	status SMALLINT DEFAULT 0,
	cooldown_until TIMESTAMPTZ,
	disable_reason TEXT,
	last_used_at TIMESTAMPTZ,
	updated_at TIMESTAMPTZ DEFAULT NOW()
) WITH (fillfactor=90)`
	}
	if _, err := db.Exec(ddl); err != nil {
		return err
	}
	if err := dropLegacySecondaryIndexes(db, d); err != nil {
		return err
	}
	/* 为旧表迁移：添加新的状态列 */
	if err := migrateAddStatusColumns(db, d); err != nil {
		return err
	}
	return nil
}

/* migrateAddStatusColumns 为旧表添加新的状态列 */
func migrateAddStatusColumns(db *sql.DB, d Dialect) error {
	switch d {
	case DialectMySQL:
		newCols := []string{"status", "cooldown_until", "disable_reason", "last_used_at"}
		for _, col := range newCols {
			if err := addColumnIfNotExists(db, d, col); err != nil {
				return fmt.Errorf("add column %s: %w", col, err)
			}
		}
	case DialectSQLite:
		newCols := []string{"status", "cooldown_until", "disable_reason", "last_used_at"}
		for _, col := range newCols {
			if err := addColumnIfNotExists(db, d, col); err != nil {
				return fmt.Errorf("add column %s: %w", col, err)
			}
		}
	default: /* PostgreSQL */
		newCols := []string{"status", "cooldown_until", "disable_reason", "last_used_at"}
		for _, col := range newCols {
			if err := addColumnIfNotExists(db, d, col); err != nil {
				return fmt.Errorf("add column %s: %w", col, err)
			}
		}
	}
	return nil
}

func addColumnIfNotExists(db *sql.DB, d Dialect, colName string) error {
	var colDef string
	switch colName {
	case "status":
		switch d {
		case DialectMySQL:
			colDef = "TINYINT DEFAULT 0"
		case DialectSQLite:
			colDef = "INTEGER DEFAULT 0"
		default: /* PostgreSQL */
			colDef = "SMALLINT DEFAULT 0"
		}
	case "cooldown_until":
		switch d {
		case DialectMySQL:
			colDef = "DATETIME(6) NULL"
		case DialectSQLite:
			colDef = "TEXT"
		default: /* PostgreSQL */
			colDef = "TIMESTAMPTZ"
		}
	case "disable_reason":
		colDef = "VARCHAR(128)"
	case "last_used_at":
		switch d {
		case DialectMySQL:
			colDef = "DATETIME(6) NULL"
		case DialectSQLite:
			colDef = "TEXT"
		default: /* PostgreSQL */
			colDef = "TIMESTAMPTZ"
		}
	}

	switch d {
	case DialectMySQL:
		_, _ = db.Exec(fmt.Sprintf("ALTER TABLE codex_accounts ADD COLUMN %s %s", mysqlBacktickIdent(colName), colDef))
	case DialectSQLite:
		_, _ = db.Exec(fmt.Sprintf("ALTER TABLE codex_accounts ADD COLUMN %s %s", colName, colDef))
	default: /* PostgreSQL */
		_, _ = db.Exec(fmt.Sprintf("ALTER TABLE codex_accounts ADD COLUMN IF NOT EXISTS %s %s", pqQuoteIdent(colName), colDef))
	}
	return nil
}

/* 旧版本曾建 refresh_token / updated_at 二级索引，业务查询未使用，删除以降低写放大 */
func dropLegacySecondaryIndexes(db *sql.DB, d Dialect) error {
	switch d {
	case DialectMySQL:
		for _, idx := range []string{"idx_codex_accounts_refresh_token", "idx_codex_accounts_updated_at"} {
			_, err := db.Exec(fmt.Sprintf("ALTER TABLE codex_accounts DROP INDEX %s", mysqlBacktickIdent(idx)))
			if err != nil && !mysqlDropIndexMissing(err) {
				return fmt.Errorf("drop index %s: %w", idx, err)
			}
		}
	case DialectSQLite:
		for _, idx := range []string{"idx_codex_accounts_refresh_token", "idx_codex_accounts_updated_at"} {
			if _, err := db.Exec(fmt.Sprintf("DROP INDEX IF EXISTS %s", idx)); err != nil {
				return fmt.Errorf("drop index %s: %w", idx, err)
			}
		}
	default:
		for _, idx := range []string{"idx_codex_accounts_refresh_token", "idx_codex_accounts_updated_at"} {
			if _, err := db.Exec(fmt.Sprintf("DROP INDEX IF EXISTS %s", pqQuoteIdent(idx))); err != nil {
				return fmt.Errorf("drop index %s: %w", idx, err)
			}
		}
	}
	return nil
}

func mysqlDropIndexMissing(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "1091") || strings.Contains(s, "check that column/key exists") || strings.Contains(s, "doesn't exist")
}
