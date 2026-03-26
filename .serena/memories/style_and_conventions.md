# style and conventions
- Assistant/user-facing replies default to Simplified Chinese; keep code identifiers, commands, logs, and protocol field names in original language.
- Follow KISS, YAGNI, DRY, SOLID from project instructions.
- Keep code UTF-8 without BOM.
- Existing code style prefers small focused helpers, explicit config structs, and concise Chinese comments for non-obvious behavior.
- Preserve established package boundaries: proxy/dial/transport helpers in `internal/netutil`, auth concerns in `internal/auth`, execution flow in `internal/executor`.
- Prefer shared transport/dial utilities over duplicating HTTP transport setup.
- Avoid over-design; implement only the behavior needed by current config/runtime paths.