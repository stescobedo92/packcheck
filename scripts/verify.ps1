# PackCheck - full verification on Windows (PowerShell).
# Exits non-zero when any gate fails.
# Native commands are governed by $LASTEXITCODE (checked after each one):
# with 'Stop', PowerShell turns any stderr output (e.g. the normal gosec
# logs) into a terminating error even when the process exited 0.
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path $PSScriptRoot -Parent)

Write-Host '== gofmt =='
$unformatted = gofmt -l .
if ($unformatted) { throw "Unformatted files: $unformatted" }

Write-Host '== go vet =='
go vet ./...
if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }

Write-Host '== go test (unit + E2E integration) =='
go test ./... -count=1
if ($LASTEXITCODE -ne 0) { throw 'go test failed' }

Write-Host '== evidence batch (real CLI) =='
go build -o packcheck.exe ./cmd/packcheck
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
.\packcheck.exe seed --db evidence\packcheck.db
if ($LASTEXITCODE -ne 0) { throw 'seed failed' }
.\packcheck.exe runbatch --db evidence\packcheck.db --out evidence
if ($LASTEXITCODE -ne 0) { throw 'runbatch did not match expectations' }

Write-Host '== analyzers (optional, when installed) =='
foreach ($tool in @('staticcheck', 'gosec', 'govulncheck')) {
    $exe = Join-Path $env:USERPROFILE "go\bin\$tool.exe"
    if (Test-Path $exe) {
        Write-Host "-- $tool"
        & $exe ./... 2>&1 | Select-Object -Last 3
        if ($LASTEXITCODE -ne 0) { throw "$tool failed" }
    } else {
        Write-Host "-- $tool not installed (use go install to enable it); skipped without faking a result"
    }
}

Write-Host 'FULL VERIFICATION: OK'
