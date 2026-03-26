# suggested commands
- List files (PowerShell): `Get-ChildItem`
- Search text: `rg "pattern"`
- Search files: `rg --files`
- Format Go files: `gofmt -w <files>`
- Run all tests on Windows sandbox safely: `$env:GOCACHE = Join-Path (Get-Location) '.gocache'; go test ./...`
- Run the service: `go run . -config config.yaml`
- Build binary: `go build ./...`
- Check git status: `git status --short`
- Show diff summary: `git diff --stat`