# task completion checklist
- Format edited Go files with `gofmt -w`.
- Run verification with `$env:GOCACHE = Join-Path (Get-Location) '.gocache'; go test ./...` because default Go build cache may be blocked by sandbox permissions.
- Review `git diff --stat` / `git status --short` for accidental artifacts before finalizing.
- Mention any generated workspace artifacts (for example `.gocache/`) if they were created during verification.